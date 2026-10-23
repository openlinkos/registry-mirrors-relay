package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/manifest"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/testutil"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
	"github.com/openlinkos/registry-mirrors-relay/internal/web"
)

type collector struct {
	mu      sync.Mutex
	entries []accesslog.Entry
}

func (c *collector) Log(e *accesslog.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, *e)
}

func (c *collector) last() accesslog.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[len(c.entries)-1]
}

type env struct {
	srv  *httptest.Server
	reg  *testutil.Registry // registry.test：令牌在 auth.test，blob 在 cdn.test
	hub  *testutil.Registry // hubreg.test：模拟 Docker Hub（library 前缀）
	img  *testutil.Image
	alp  *testutil.Image
	logs *collector
	cfg  *config.Config
}

const testConfig = `
client_ip:
  trusted_proxies: [loopback]
entrances:
  - hosts: [mirror.local]
    default_upstream: hub.test
  - hosts: [reg.local]
    upstream: registry.test
upstreams:
  registry.test:
    registry: registry.test
    redirect_hosts: ['cdn\.test', 'pkg\.test']
    token_realm_hosts: ['auth\.test']
  hub.test:
    registry: hubreg.test
    library_prefix: true
    redirect_hosts: ['hubcdn\.test']
    token_realm_hosts: ['hubauth\.test']
timeouts:
  upstream_read_idle: 2s
  write_idle: 2s
`

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	n := testutil.NewNet(t)
	e := &env{logs: &collector{}}
	e.reg = testutil.NewRegistry(t, n, "registry.test", "auth.test", "cdn.test")
	e.hub = testutil.NewRegistry(t, n, "hubreg.test", "hubauth.test", "hubcdn.test")
	e.img = testutil.NewImage(t, "team/app", "v1", 256<<10)
	e.reg.Add(e.img)
	e.alp = testutil.NewImage(t, "library/alpine", "3.22", 32<<10)
	e.hub.Add(e.alp)
	res, err := config.Parse([]byte(testConfig + extra))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = res.Config
	log := slog.New(slog.DiscardHandler)
	pool, err := upstream.NewPool(e.cfg, upstream.Options{DialContext: n.DialContext, RootCAs: n.CA.Pool, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	trusted, _ := config.ParsePrefixes(e.cfg.ClientIP.TrustedProxies)
	wh, err := web.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Config: e.cfg, Router: route.New(e.cfg), Pool: pool, IPs: clientip.New(trusted, false), Web: wh, Access: e.logs, Log: log,
		Manifests: manifest.New(e.cfg.Cache, e.cfg.Limits.MaxManifestBuffers, 10*time.Second)})
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(t *testing.T, method, host, path string, hdr http.Header) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s：%v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) layer() string {
	for d, b := range e.img.Blobs {
		if len(b) == 256<<10 {
			return d
		}
	}
	return ""
}

func TestPullFixedEntrance(t *testing.T) {
	e := newEnv(t, "")
	p := &testutil.Puller{Base: e.srv.URL, Host: "reg.local"}
	dg, n, err := p.Pull("team/app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if dg != e.img.IndexDigest() || n < 4*256<<10 {
		t.Fatalf("digest %s，字节 %d", dg, n)
	}
	if e.reg.AuthCount("cdn.test") != 0 {
		t.Fatal("令牌不能发给 CDN")
	}
	last := e.logs.last()
	if last.Upstream != "registry.test" || len(last.Hops) != 1 || last.Hops[0] != "cdn.test" || last.Status != 200 || last.Size == 0 {
		t.Fatalf("访问日志：%+v", last)
	}
}

func TestPullAllInOne(t *testing.T) {
	e := newEnv(t, "")
	// 根路径：默认上游，只有一段的名字补 library/
	p := &testutil.Puller{Base: e.srv.URL, Host: "mirror.local"}
	if dg, _, err := p.Pull("alpine", "3.22"); err != nil || dg != e.alp.IndexDigest() {
		t.Fatalf("根路径：%s %v", dg, err)
	}
	// 路径前缀
	if dg, _, err := p.Pull("registry.test/team/app", "v1"); err != nil || dg != e.img.IndexDigest() {
		t.Fatalf("路径前缀：%s %v", dg, err)
	}
	// containerd 的 ns
	p.Header = http.Header{}
	resp, _ := e.do(t, http.MethodHead, "mirror.local", "/v2/team/app/manifests/v1?ns=registry.test", http.Header{"Accept": {testutil.MediaIndex}})
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Content-Digest") != e.img.IndexDigest() {
		t.Fatalf("ns：%d %v", resp.StatusCode, resp.Header)
	}
}

func TestResponseHeaders(t *testing.T) {
	e := newEnv(t, "")
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", http.Header{"Accept": {testutil.MediaIndex}, "Cookie": {"a=b"}})
	h := resp.Header
	for _, k := range []string{"Set-Cookie", "Docker-Ratelimit-Source", "Ratelimit-Remaining", "Alt-Svc"} {
		if h.Get(k) != "" {
			t.Errorf("不应转给客户端：%s", k)
		}
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("缺少安全头：%v", h)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("按 tag 的 manifest 不能被缓存：%q", h.Get("Cache-Control"))
	}
	if h.Get("Content-Type") != testutil.MediaIndex || h.Get("Docker-Content-Digest") != e.img.IndexDigest() {
		t.Errorf("manifest 头：%v", h)
	}
	l := e.layer()
	resp, _ = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+l, nil)
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Docker-Content-Digest") != l {
		t.Errorf("blob 头：%v", resp.Header)
	}
	// 上游的 Ratelimit 头可以按配置转发（只有直接转发的响应才有，这里关掉 manifest 缓存）
	e2 := newEnv(t, "proxy:\n  expose_upstream_ratelimit: true\ncache:\n  max_bytes: 0\n")
	resp, _ = e2.do(t, http.MethodHead, "reg.local", "/v2/team/app/manifests/v1", nil)
	if resp.Header.Get("Ratelimit-Remaining") == "" || resp.Header.Get("Docker-Ratelimit-Source") != "" {
		t.Errorf("expose_upstream_ratelimit：%v", resp.Header)
	}
}

func TestRangeAndBlobHead(t *testing.T) {
	e := newEnv(t, "")
	l := e.layer()
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+l, http.Header{"Range": {"bytes=100-199"}})
	if resp.StatusCode != http.StatusPartialContent || len(body) != 100 || string(body) != string(e.img.Blobs[l][100:200]) {
		t.Fatalf("Range：%d %d", resp.StatusCode, len(body))
	}
	// HEAD 跟随到 CDN 时改成 Range 0-0 探测（CDN 的签名只对 GET 有效）
	resp, body = e.do(t, http.MethodHead, "reg.local", "/v2/team/app/blobs/"+l, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != strconv.Itoa(256<<10) || len(body) != 0 || resp.Header.Get("Content-Range") != "" {
		t.Fatalf("blob HEAD：%d %v", resp.StatusCode, resp.Header)
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t, "")
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/missing/manifests/v1", nil)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "MANIFEST_UNKNOWN") {
		t.Fatalf("不存在的仓库：%d %s", resp.StatusCode, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/busy"] = testutil.FailWith{Status: 429,
		Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}}, Body: `{"errors":[{"code":"TOOMANYREQUESTS","message":"slow down"}]}`}
	resp, body = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/busy", nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || !strings.Contains(string(body), "TOOMANYREQUESTS") {
		t.Fatalf("429 和 Retry-After 应转给客户端：%d %v %s", resp.StatusCode, resp.Header, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/html"] = testutil.FailWith{Status: 503,
		Header: http.Header{"Content-Type": {"text/html"}}, Body: "<html>cloudflare</html>"}
	resp, body = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/html", nil)
	if resp.StatusCode != 503 || strings.Contains(string(body), "<html>") || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("HTML 错误页应换成 OCI JSON：%d %s", resp.StatusCode, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/private"] = testutil.FailWith{Status: 401,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: `{"errors":[{"code":"UNAUTHORIZED"}]}`}
	resp, _ = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/private", nil)
	if resp.StatusCode != 404 || resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("上游最终的 401 应变成 404 且不带质询：%d %v", resp.StatusCode, resp.Header)
	}
}

func TestEntranceRules(t *testing.T) {
	e := newEnv(t, "")
	cases := []struct {
		method, host, path string
		status             int
	}{
		{http.MethodPost, "reg.local", "/v2/team/app/blobs/uploads/", 405},
		{http.MethodPut, "reg.local", "/v2/team/app/manifests/v1", 405},
		{http.MethodDelete, "reg.local", "/v2/team/app/manifests/v1", 405},
		{http.MethodGet, "reg.local", "/v2/_catalog", 404},
		{http.MethodGet, "unknown.local", "/v2/", 404},
		{http.MethodGet, "reg.local", "/v2/library/hello/manifests/latest?ns=docker.io", 404},
		{http.MethodGet, "mirror.local", "/v2/example.org/x/manifests/latest", 404},
		{http.MethodGet, "reg.local", "/v2/", 200},
		{http.MethodGet, "reg.local", "/", 200},
		{http.MethodGet, "reg.local", "/admin", 404},
	}
	for _, c := range cases {
		resp, body := e.do(t, c.method, c.host, c.path, nil)
		if resp.StatusCode != c.status {
			t.Errorf("%s %s %s：期望 %d，实际 %d %s", c.method, c.host, c.path, c.status, resp.StatusCode, body)
		}
		if c.status == 405 && resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("405 应带 Allow")
		}
	}
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/", nil)
	if string(body) != "{}" || resp.Header.Get("Docker-Distribution-Api-Version") != "registry/2.0" || resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("/v2/ 应在本地回答，不发质询：%s %v", body, resp.Header)
	}
}

func TestUnlistedRedirectPassedToClient(t *testing.T) {
	e := newEnv(t, "")
	// 另建一个白名单不含 cdn.test 的环境
	n := testutil.NewNet(t)
	reg := testutil.NewRegistry(t, n, "registry.test", "auth.test", "cdn.test")
	reg.Add(e.img)
	res, _ := config.Parse([]byte(strings.Replace(testConfig, `['cdn\.test', 'pkg\.test']`, `['pkg\.test']`, 1)))
	p, _ := upstream.NewPool(res.Config, upstream.Options{DialContext: n.DialContext, RootCAs: n.CA.Pool, Log: slog.New(slog.DiscardHandler)})
	wh, _ := web.New(res.Config)
	logs := &collector{}
	h := New(Deps{Config: res.Config, Router: route.New(res.Config), Pool: p, IPs: clientip.New(nil, false), Web: wh, Access: logs, Log: slog.New(slog.DiscardHandler)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team/app/blobs/"+e.layer(), nil)
	req.Host = "reg.local"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect || !strings.HasPrefix(rec.Header().Get("Location"), "https://cdn.test/") {
		t.Fatalf("白名单外的重定向应交给客户端：%d %v", rec.Code, rec.Header())
	}
}

func TestClientIPInLog(t *testing.T) {
	e := newEnv(t, "")
	e.do(t, http.MethodGet, "reg.local", "/v2/", http.Header{"X-Forwarded-For": {"10.0.0.9, 198.51.100.23"}, "User-Agent": {"containerd/2.1"}})
	last := e.logs.last()
	if last.ClientIP != "198.51.100.23" || last.RemoteIP != "127.0.0.1" || last.UserAgent != "containerd/2.1" || last.Entrance != "reg.local" {
		t.Fatalf("访问日志：%+v", last)
	}
	var m map[string]any
	if err := json.Unmarshal(accesslog.Marshal(&last), &m); err != nil || m["status"].(float64) != 200 {
		t.Fatalf("日志 JSON：%v %v", m, err)
	}
}

func TestTagsLinkRewrite(t *testing.T) {
	e := newEnv(t, "")
	resp, body := e.do(t, http.MethodGet, "mirror.local", "/v2/registry.test/team/app/tags/list?n=1", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"v1"`) {
		t.Fatalf("tags：%d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Link"); got != `</v2/registry.test/team/app/tags/list?last=v1&n=1>; rel="next"` {
		t.Fatalf("Link：%q", got)
	}
}

// 上游中途没有数据：在 upstream_read_idle 之后放弃，连接被中断，客户端看到的是截断而不是完整的响应
type stallReader struct {
	ctx  context.Context
	sent bool
}

func (s *stallReader) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(p, "partial"), nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func TestStreamUpstreamIdle(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Timeouts.UpstreamReadIdle = config.Duration(200 * time.Millisecond)
	h := &Handler{Deps: Deps{Config: e.cfg}}
	h.bufs.New = func() any { b := make([]byte, 1024); return &b }
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	start := time.Now()
	err := h.stream(rec, &stallReader{ctx: ctx}, cancel)
	if !errors.Is(err, errUpstreamIdle) || time.Since(start) > 2*time.Second || rec.Body.String() != "partial" {
		t.Fatalf("应在空闲超时后放弃：%v %v %q", err, time.Since(start), rec.Body.String())
	}
}

func TestManifestCacheEndToEnd(t *testing.T) {
	e := newEnv(t, "")
	p := &testutil.Puller{Base: e.srv.URL, Host: "reg.local"}
	if _, _, err := p.Pull("team/app", "v1"); err != nil {
		t.Fatal(err)
	}
	gets := e.reg.Count("GET registry.test manifest")
	if _, _, err := p.Pull("team/app", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := e.reg.Count("GET registry.test manifest"); got != gets {
		t.Fatalf("第二次拉取不应再向上游 GET manifest：之前 %d，之后 %d", gets, got)
	}
	hits := 0
	e.logs.mu.Lock()
	for _, l := range e.logs.entries {
		if l.Cache == "hit" {
			hits++
		}
	}
	e.logs.mu.Unlock()
	if hits < 3 {
		t.Fatalf("第二次拉取的 index 和两个平台的 manifest 都应命中缓存：%d", hits)
	}
}

func TestStaleManifestWhenUpstreamFails(t *testing.T) {
	e := newEnv(t, "cache:\n  tag_ttl: 1ms\n")
	p := &testutil.Puller{Base: e.srv.URL, Host: "reg.local"}
	if _, _, err := p.Pull("team/app", "v1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	e.reg.Fail["registry.test /v2/team/app/manifests/v1"] = testutil.FailWith{Status: 503}
	// Accept 和拉取客户端用的一样：tag 缓存的键里带 Accept
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", http.Header{"Accept": {testutil.MediaIndex + ", " + testutil.MediaManifest}})
	if resp.StatusCode != 200 || string(body) != string(e.img.Index) || e.logs.last().Cache != "stale" {
		t.Fatalf("上游 503 时应返回旧的 manifest：%d %q", resp.StatusCode, e.logs.last().Cache)
	}
}
