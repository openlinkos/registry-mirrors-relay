package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/limit"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/testutil"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
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
	app  *app.App
	net  *testutil.Net
}

const testConfig = `
client_ip:
  trusted_proxies: [loopback]
entrances:
  - hosts: [mirror.local]
    default_upstream: hub.test
  - hosts: [reg.local]
    upstream: registry.test
  - hosts: [private.local]
    upstream: registry.test
    auth: basic
  - hosts: [hidden.local]
    upstream: registry.test
    auth: basic
    page: login
  - hosts: [bare.local]
    upstream: registry.test
    page: off
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
log:
  access: {output: "off"}
`

func newEnv(t *testing.T, extra string) *env { return newEnvStore(t, extra, store.NewReadOnly()) }

// newEnvStore 用给定的运营数据存储建测试环境。
func newEnvStore(t *testing.T, extra string, st store.Store) *env {
	t.Helper()
	n := testutil.NewNet(t)
	e := &env{logs: &collector{}, net: n}
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
	e.app, err = app.New(res, nil, app.Options{
		Upstream: upstream.Options{DialContext: n.DialContext, RootCAs: n.CA.Pool, Log: log},
		Log:      log, Store: st, Hook: e.logs.Log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.app.Close)
	e.srv = httptest.NewServer(New(Deps{App: e.app, Log: log}))
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
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Do(req)
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
	// 白名单里去掉 cdn.test
	e := newEnv(t, "")
	e.cfg.Upstreams["registry.test"].RedirectHosts = []string{`pkg\.test`}
	if err := e.app.Rebuild(); err != nil {
		t.Fatal(err)
	}
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil)
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(resp.Header.Get("Location"), "https://cdn.test/") {
		t.Fatalf("白名单外的重定向应交给客户端：%d %v", resp.StatusCode, resp.Header)
	}
	if e.app.Stats.Unlisted()["registry.test"]["cdn.test"] != 1 {
		t.Fatalf("应记下白名单外的主机：%v", e.app.Stats.Unlisted())
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
	h := &Handler{Deps: Deps{App: e.app}}
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

func boltEnv(t *testing.T, extra string) (*env, *store.Bolt) {
	t.Helper()
	st, err := store.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return newEnvStore(t, extra, st), st
}

func (e *env) update(t *testing.T, fn func(*store.Data)) {
	t.Helper()
	if err := e.app.Store.Update(func(d *store.Data) error { fn(d); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Rebuild(); err != nil {
		t.Fatal(err)
	}
}

func basic(user, pw string) http.Header {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, pw)
	return http.Header{"Authorization": r.Header["Authorization"]}
}

func TestBasicAuthEntrance(t *testing.T) {
	e, _ := boltEnv(t, "")
	h, _ := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	e.update(t, func(d *store.Data) { d.Users = append(d.Users, store.ClientUser{Name: "alice", Hash: string(h)}) })

	resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", nil)
	if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic ") {
		t.Fatalf("没带口令应返回 Basic 质询：%d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "wrong")); resp.StatusCode != 401 {
		t.Fatalf("口令错误应返回 401：%d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "correct horse")); resp.StatusCode != 200 {
		t.Fatalf("口令正确应返回 200：%d", resp.StatusCode)
	}
	p := &testutil.Puller{Base: e.srv.URL, Host: "private.local", Header: basic("alice", "correct horse")}
	if _, _, err := p.Pull("team/app", "v1"); err != nil {
		t.Fatalf("登录后应能拉取：%v", err)
	}
	if e.logs.last().UserID != "alice" {
		t.Fatalf("访问日志应记下用户：%+v", e.logs.last())
	}
	// 用法页默认谁都能看，只有拉取要登录；site.json 告诉页面这里要登录
	if resp, body := e.do(t, http.MethodGet, "private.local", "/", nil); resp.StatusCode != 200 || !strings.Contains(string(body), "<html") {
		t.Fatalf("page 默认 public，没登录也应显示用法页：%d", resp.StatusCode)
	}
	if resp, body := e.do(t, http.MethodGet, "private.local", "/site.json", nil); resp.StatusCode != 200 || !strings.Contains(string(body), `"login": true`) {
		t.Fatalf("要登录的入口的 site.json 应带 login: true：%d %s", resp.StatusCode, body)
	}
	// page: login：用法页也要登录
	if resp, _ := e.do(t, http.MethodGet, "hidden.local", "/", nil); resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic ") {
		t.Fatalf("page: login 时没登录应返回 Basic 质询：%d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodGet, "hidden.local", "/status.json", nil); resp.StatusCode != 401 {
		t.Fatalf("page: login 时 status.json 也要登录：%d", resp.StatusCode)
	}
	if resp, body := e.do(t, http.MethodGet, "hidden.local", "/", basic("alice", "correct horse")); resp.StatusCode != 200 || !strings.Contains(string(body), "<html") {
		t.Fatalf("page: login 时登录后应显示用法页：%d", resp.StatusCode)
	}
	// page: off：没有用法页，拉取照常
	for _, path := range []string{"/", "/site.json", "/status.json"} {
		if resp, _ := e.do(t, http.MethodGet, "bare.local", path, nil); resp.StatusCode != 404 {
			t.Fatalf("page: off 时 %s 应返回 404：%d", path, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, http.MethodGet, "bare.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatalf("page: off 不影响拉取：%d", resp.StatusCode)
	}
	// 公开入口不受影响
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatal("公开入口不需要登录")
	}
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/", nil); resp.StatusCode != 200 {
		t.Fatal("公开入口的用法页不需要登录")
	}
	// 停用账号
	e.update(t, func(d *store.Data) { d.Users[0].Disabled = true })
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "correct horse")); resp.StatusCode != 401 {
		t.Fatalf("停用的账号应返回 401：%d", resp.StatusCode)
	}
	// 失败次数限制（默认每分钟 10 次）。用已有账号输错口令：它的哈希 cost 很低，-race 下也很快，
	// 不会因为测试太慢、令牌在这期间补回来而测不出限制
	e.update(t, func(d *store.Data) { d.Users[0].Disabled = false })
	var last int
	for i := 0; i < 12; i++ {
		resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "wrong"))
		last = resp.StatusCode
	}
	if last != 429 {
		t.Fatalf("登录失败太多次应返回 429：%d", last)
	}
}

func TestIPBan(t *testing.T) {
	e, _ := boltEnv(t, "")
	e.update(t, func(d *store.Data) {
		d.Bans = append(d.Bans, store.Ban{CIDR: "127.0.0.1/32", Until: time.Now().Add(time.Hour), Reason: "test"})
	})
	for _, path := range []string{"/v2/", "/"} {
		if resp, _ := e.do(t, http.MethodGet, "reg.local", path, nil); resp.StatusCode != 403 {
			t.Fatalf("被封禁的 IP 访问 %s 应返回 403：%d", path, resp.StatusCode)
		}
	}
	if e.logs.last().Error != "banned" {
		t.Fatalf("日志应记下原因：%+v", e.logs.last())
	}
	e.update(t, func(d *store.Data) { d.Bans = nil })
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatal("解除封禁后应恢复")
	}
}

func TestImageDenyRule(t *testing.T) {
	e := newEnv(t, `access:
  images:
    deny: [{pattern: 'registry.test/team/**', status: 451, message: blocked}]
`)
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != 451 || !strings.Contains(string(body), "blocked") || !strings.Contains(string(body), "DENIED") {
		t.Fatalf("黑名单里的镜像应返回 451：%d %s", resp.StatusCode, body)
	}
	if e.reg.Count("GET registry.test manifest") != 0 {
		t.Fatal("被拒绝的请求不应发给上游")
	}
	if resp, _ := e.do(t, http.MethodGet, "mirror.local", "/v2/alpine/manifests/3.22", nil); resp.StatusCode != 200 {
		t.Fatalf("别的镜像不受影响：%d", resp.StatusCode)
	}
}

func TestManifestRateLimit(t *testing.T) {
	e := newEnv(t, `limits:
  manifest_rate: 0.5
  manifest_burst: 2
`)
	codes := []int{}
	for i := 0; i < 3; i++ {
		resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", nil)
		codes = append(codes, resp.StatusCode)
		if i == 2 && resp.Header.Get("Retry-After") == "" {
			t.Fatal("429 应带 Retry-After")
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || !e.logs.last().Limited {
		t.Fatalf("突发 2 次之后应返回 429：%v", codes)
	}
}

func TestBlobStreamLimit(t *testing.T) {
	e := newEnv(t, `limits:
  blob_streams_per_client: 1
`)
	// 先占住这个客户端唯一的名额，模拟一个还在下载的层
	rel, ok, _ := e.app.Limiter.AcquireBlob("ip:127.0.0.1", netip.MustParseAddr("127.0.0.1"), limitOverrideNone)
	if !ok {
		t.Fatal("应能占到名额")
	}
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil)
	if resp.StatusCode != 429 {
		t.Fatalf("超过每个客户端的并发流数应返回 429：%d", resp.StatusCode)
	}
	rel()
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil); resp.StatusCode != 200 {
		t.Fatalf("释放后应能下载：%d", resp.StatusCode)
	}
}

var limitOverrideNone = limit.Override{}
