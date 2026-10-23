package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/testutil"
)

type env struct {
	net  *testutil.Net
	reg  *testutil.Registry
	img  *testutil.Image
	pool *Pool
	up   *Upstream
}

// newEnv 建一个假上游 registry.test（令牌在 auth.test，blob 在 cdn.test）和指向它的 Pool。
func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	n := testutil.NewNet(t)
	reg := testutil.NewRegistry(t, n, "registry.test", "auth.test", "cdn.test")
	img := testutil.NewImage(t, "team/app", "v1", 64<<10)
	reg.Add(img)
	t.Setenv("TEST_PW", "pw")
	res, err := config.Parse([]byte(`
upstreams:
  registry.test:
    registry: registry.test
    redirect_hosts: ['cdn\.test', 'pkg\.test']
    token_realm_hosts: ['auth\.test']
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(res.Config, Options{DialContext: n.DialContext, RootCAs: n.CA.Pool, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &env{net: n, reg: reg, img: img, pool: pool, up: pool.Get("registry.test")}
}

func (e *env) fetch(t *testing.T, method, path string, h http.Header) (*Response, []byte) {
	t.Helper()
	if h == nil {
		h = http.Header{}
	}
	resp, err := e.up.Fetch(context.Background(), Request{Method: method, Path: path, Header: h, Repo: "team/app"})
	if err != nil {
		t.Fatalf("Fetch %s：%v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestTokenDanceAndReuse(t *testing.T) {
	e := newEnv(t, "")
	path := "/v2/team/app/manifests/v1"
	resp, body := e.fetch(t, http.MethodGet, path, http.Header{"Accept": {testutil.MediaIndex}})
	if resp.StatusCode != 200 || !bytes.Equal(body, e.img.Index) {
		t.Fatalf("应拿到 index：%d", resp.StatusCode)
	}
	if got := e.reg.Count("GET registry.test manifest"); got != 2 {
		t.Fatalf("第一次：401 之后取令牌重试，应请求 2 次，实际 %d", got)
	}
	e.fetch(t, http.MethodGet, path, nil)
	if got := e.reg.Count("GET registry.test manifest"); got != 3 {
		t.Fatalf("第二次应直接带上缓存的令牌，只请求 1 次，实际累计 %d", got)
	}
	if got := e.reg.Count("GET auth.test token"); got != 1 {
		t.Fatalf("令牌应只取 1 次，实际 %d", got)
	}
	// 换一个仓库：带上新仓库的 scope 取令牌，不复用上一个仓库的
	other := testutil.NewImage(t, "team/other", "v2", 1024)
	e.reg.Add(other)
	resp, err := e.up.Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/other/manifests/v2", Header: http.Header{}, Repo: "team/other"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("另一个仓库：%v %v", resp, err)
	}
	resp.Body.Close()
	if resp.Header.Get("Docker-Content-Digest") != other.IndexDigest() {
		t.Fatal("HEAD 应带 digest")
	}
	if got := e.reg.Count("HEAD registry.test manifest"); got != 1 {
		t.Fatalf("已知质询时应主动带上新仓库的令牌，HEAD 只发 1 次，实际 %d", got)
	}
}

func TestBlobRedirectFollowedWithoutAuthorization(t *testing.T) {
	e := newEnv(t, "")
	var layer string
	for d, b := range e.img.Blobs {
		if len(b) == 64<<10 {
			layer = d
			break
		}
	}
	resp, body := e.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if resp.StatusCode != 200 || testutil.Digest(body) != layer {
		t.Fatalf("应经 CDN 拿到完整的层：%d", resp.StatusCode)
	}
	if len(resp.Hops) != 1 || resp.Hops[0] != "cdn.test" {
		t.Fatalf("hops = %v", resp.Hops)
	}
	if e.reg.AuthCount("cdn.test") != 0 {
		t.Fatal("跟随重定向时不能把令牌发给 CDN")
	}
	// Range 透传
	resp, body = e.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, http.Header{"Range": {"bytes=10-19"}})
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 || !bytes.Equal(body, e.img.Blobs[layer][10:20]) {
		t.Fatalf("Range：%d %d", resp.StatusCode, len(body))
	}
}

func TestUnlistedRedirect(t *testing.T) {
	pass := newEnv(t, "    extra_redirect_hosts: []\n")
	pass.up.redirectRe, _ = config.CompileHostPatterns([]string{`pkg\.test`}) // 去掉 cdn.test
	var layer string
	for d := range pass.img.Blobs {
		layer = d
		break
	}
	resp, _ := pass.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if !strings.HasPrefix(resp.PassLocation, "https://cdn.test/blobs/") || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("白名单外应原样交给客户端：%+v", resp)
	}

	deny := newEnv(t, "    unlisted_redirect: deny\n")
	deny.up.redirectRe = pass.up.redirectRe
	deny.reg.Add(pass.img)
	_, err := deny.up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/team/app/blobs/" + layer, Header: http.Header{}, Repo: "team/app"})
	var re *RedirectError
	if !errors.As(err, &re) || re.Host != "cdn.test" {
		t.Fatalf("deny 时应返回 RedirectError：%v", err)
	}

	zero := newEnv(t, "")
	zero.reg.Add(pass.img)
	zero.up.maxRedirects = 0
	resp, _ = zero.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if resp.PassLocation == "" {
		t.Fatal("超过跳数应交给客户端")
	}
}

func TestChallengeAfterRedirect(t *testing.T) {
	// 模拟 registry.k8s.io：manifest 先 307 到 pkg.test，那边才要求令牌
	e := newEnv(t, `    credentials:
      username: bot
      password: {env: TEST_PW}
      realm_host: auth.test
      public_only: true
`)
	e.reg.ServeManifestHost(t, e.net, "pkg.test")
	resp, body := e.fetch(t, http.MethodGet, "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, e.img.Index) {
		t.Fatalf("应在跳转后按 pkg.test 的质询取令牌：%d %s", resp.StatusCode, body)
	}
	if len(resp.Hops) != 1 || resp.Hops[0] != "pkg.test" {
		t.Fatalf("hops = %v", resp.Hops)
	}
	if e.reg.AuthCount("auth.test") != 0 {
		t.Fatal("跟随重定向时取令牌不能用配置的账号")
	}
	// 发给 registry 本身的请求才用账号
	var layer string
	for d := range e.img.Blobs {
		layer = d
		break
	}
	e.reg.CDNHost = ""
	e.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if e.reg.AuthCount("auth.test") != 1 {
		t.Fatalf("向 registry 取令牌时应带上账号：%d", e.reg.AuthCount("auth.test"))
	}
}

func TestRealmNotAllowed(t *testing.T) {
	e := newEnv(t, "")
	e.up.realmRe, _ = config.CompileHostPatterns([]string{`other\.test`})
	resp, _ := e.fetch(t, http.MethodGet, "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("令牌服务不在白名单时应返回上游的 401：%d", resp.StatusCode)
	}
	if e.reg.Count("GET auth.test token") != 0 {
		t.Fatal("不应访问白名单外的令牌服务")
	}
}

func TestNoLocation(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Fail["registry.test /v2/team/app/manifests/x"] = testutil.FailWith{Status: 307}
	_, err := e.up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/team/app/manifests/x", Header: http.Header{}, Repo: "team/app"})
	if !errors.Is(err, ErrNoLocation) {
		t.Fatalf("没有 Location 应报错：%v", err)
	}
}

func TestHostAllowed(t *testing.T) {
	re, _ := config.CompileHostPatterns([]string{`cdn\.test`})
	for raw, want := range map[string]bool{
		"https://cdn.test/x":          true,
		"https://CDN.test./x":         true,
		"https://cdn.test:443/x":      true,
		"http://cdn.test/x":           false,
		"https://cdn.test:8443/x":     false,
		"https://user:pw@cdn.test/x":  false,
		"https://cdn.test.evil.com/x": false,
		"https://127.0.0.1/x":         false,
		"https://[::1]/x":             false,
		"https:///x":                  false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hostAllowed(u, re); got != want {
			t.Errorf("%s：期望 %v，实际 %v", raw, want, got)
		}
	}
}

func TestBlocked(t *testing.T) {
	for s, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "224.0.0.1": true,
		"240.0.0.1": true, "255.255.255.255": true, "::1": true, "fc00::1": true, "fe80::1": true,
		"::ffff:10.0.0.1": true, "64:ff9b::a00:1": true, "::": true,
		"8.8.8.8": false, "104.16.0.1": false, "2606:4700::1": false, "::ffff:8.8.8.8": false,
	} {
		if got := Blocked(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s：期望 %v，实际 %v", s, want, got)
		}
	}
}

func TestDialGuardBlocksLoopback(t *testing.T) {
	// 不替换拨号：用真正的拨号器连本机，应被拦下
	n := testutil.NewNet(t)
	srv := n.Serve(t, http.NotFoundHandler(), "localhost")
	res, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(res.Config, Options{RootCAs: n.CA.Pool, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	up := pool.Get("docker.io")
	up.Registry = srv.Listener.Addr().String()
	_, err = up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/", Header: http.Header{}})
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("连本机应被拦下：%v", err)
	}
}

func TestBreakerOpensAndRelayFailsFast(t *testing.T) {
	n := testutil.NewNet(t) // 网络里没有 down.test：每次拨号都失败
	res, err := config.Parse([]byte(`
proxy:
  breaker_failures: 3
  breaker_cooldown: 1h
upstreams:
  down.test:
    registry: down.test
    token_realm_hosts: ['down\.test']
`))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(res.Config, Options{DialContext: n.DialContext, RootCAs: n.CA.Pool, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	up := pool.Get("down.test")
	req := Request{Method: http.MethodGet, Path: "/v2/x/manifests/latest", Header: http.Header{}, Repo: "x"}
	for i := 0; i < 3; i++ {
		if _, err := up.Fetch(context.Background(), req); err == nil {
			t.Fatal("连不上的上游应报错")
		}
	}
	_, err = up.Fetch(context.Background(), req)
	var co *CircuitOpenError
	if !errors.As(err, &co) || co.RetryAfter <= 0 || !up.BreakerOpen() {
		t.Fatalf("连续失败 3 次后应熔断：%v", err)
	}
	// 客户端自己取消的请求不计入失败
	other := pool.Get("docker.io")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 10; i++ {
		_, _ = other.Fetch(ctx, req)
	}
	if other.BreakerOpen() {
		t.Fatal("客户端取消的请求不应触发熔断")
	}
}

func TestPrivateNetworkUpstreamWithCAFile(t *testing.T) {
	n := testutil.NewNet(t)
	reg := testutil.NewRegistry(t, n, "127.0.0.1", "", "")
	img := testutil.NewImage(t, "team/app", "v1", 1024)
	reg.Add(img)
	srv := n.Serve(t, reg, "127.0.0.1")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, n.CA.PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := "upstreams:\n  lab.example:\n    registry: \"" + srv.Listener.Addr().String() + "\"\n    tls_ca_file: \"" + filepath.ToSlash(caFile) + "\"\n"
	// 不允许内网：拨号被拦下
	res, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := NewPool(res.Config, Options{Log: slog.New(slog.DiscardHandler)})
	_, err = pool.Get("lab.example").Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/app/manifests/v1", Header: http.Header{}, Repo: "team/app"})
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("默认应拒绝连内网：%v", err)
	}
	// 允许内网，并信任 tls_ca_file 里的 CA
	res, err = config.Parse([]byte(yaml + "    allow_private_network: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ = NewPool(res.Config, Options{Log: slog.New(slog.DiscardHandler)})
	resp, err := pool.Get("lab.example").Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/app/manifests/v1", Header: http.Header{}, Repo: "team/app"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("allow_private_network 后应能拉：%v %v", resp, err)
	}
	resp.Body.Close()
	// 其他上游仍然拒绝内网
	if pool.Get("docker.io").client == pool.Get("lab.example").client {
		t.Fatal("放开内网的上游应使用单独的连接池")
	}
}

func TestBlobsUseHTTP1(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		extra := ""
		if h2 {
			extra = "proxy:\n  blob_http2: true\n"
		}
		e := newEnv(t, extra)
		e.fetch(t, http.MethodGet, "/v2/team/app/manifests/v1", nil)
		var layer string
		for d := range e.img.Blobs {
			layer = d
			break
		}
		resp, err := e.up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/team/app/blobs/" + layer, Header: http.Header{}, Repo: "team/app", Blob: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := e.reg.Proto("registry.test manifest"); got != "HTTP/2.0" {
			t.Errorf("manifest 应走 HTTP/2：%s", got)
		}
		want := "HTTP/1.1"
		if h2 {
			want = "HTTP/2.0"
		}
		if got := e.reg.Proto("cdn.test blob"); got != want {
			t.Errorf("blob_http2=%v 时镜像层应走 %s：%s", h2, want, got)
		}
	}
}
