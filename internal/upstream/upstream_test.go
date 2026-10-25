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

// newEnv builds a fake upstream registry.test (tokens on auth.test, blobs on cdn.test) and a Pool pointing
// to it.
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
		t.Fatalf("Fetch %s: %v", path, err)
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
		t.Fatalf("should get the index: %d", resp.StatusCode)
	}
	if got := e.reg.Count("GET registry.test manifest"); got != 2 {
		t.Fatalf("first: after 401, fetch a token and retry, so 2 requests expected, got %d", got)
	}
	e.fetch(t, http.MethodGet, path, nil)
	if got := e.reg.Count("GET registry.test manifest"); got != 3 {
		t.Fatalf("second: should attach the cached token directly with only 1 request, got %d in total", got)
	}
	if got := e.reg.Count("GET auth.test token"); got != 1 {
		t.Fatalf("the token should be fetched only once, got %d", got)
	}
	// Another repository: fetch a token with the new repository's scope, not reusing the previous one's
	other := testutil.NewImage(t, "team/other", "v2", 1024)
	e.reg.Add(other)
	resp, err := e.up.Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/other/manifests/v2", Header: http.Header{}, Repo: "team/other"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("other repository: %v %v", resp, err)
	}
	resp.Body.Close()
	if resp.Header.Get("Docker-Content-Digest") != other.IndexDigest() {
		t.Fatal("HEAD should carry the digest")
	}
	if got := e.reg.Count("HEAD registry.test manifest"); got != 1 {
		t.Fatalf("with a known challenge, the new repository's token should be attached up front with only 1 HEAD, got %d", got)
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
		t.Fatalf("should get the complete layer via the CDN: %d", resp.StatusCode)
	}
	if len(resp.Hops) != 1 || resp.Hops[0] != "cdn.test" {
		t.Fatalf("hops = %v", resp.Hops)
	}
	if e.reg.AuthCount("cdn.test") != 0 {
		t.Fatal("tokens must not be sent to the CDN when following redirects")
	}
	// Range passed through
	resp, body = e.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, http.Header{"Range": {"bytes=10-19"}})
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 || !bytes.Equal(body, e.img.Blobs[layer][10:20]) {
		t.Fatalf("Range: %d %d", resp.StatusCode, len(body))
	}
}

func TestUnlistedRedirect(t *testing.T) {
	pass := newEnv(t, "    extra_redirect_hosts: []\n")
	pass.up.redirectRe, _ = config.CompileHostPatterns([]string{`pkg\.test`}) // remove cdn.test
	var layer string
	for d := range pass.img.Blobs {
		layer = d
		break
	}
	resp, _ := pass.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if !strings.HasPrefix(resp.PassLocation, "https://cdn.test/blobs/") || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("outside the allowlist, it should be passed to the client as is: %+v", resp)
	}

	deny := newEnv(t, "    unlisted_redirect: deny\n")
	deny.up.redirectRe = pass.up.redirectRe
	deny.reg.Add(pass.img)
	_, err := deny.up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/team/app/blobs/" + layer, Header: http.Header{}, Repo: "team/app"})
	var re *RedirectError
	if !errors.As(err, &re) || re.Host != "cdn.test" {
		t.Fatalf("deny should return a RedirectError: %v", err)
	}

	zero := newEnv(t, "")
	zero.reg.Add(pass.img)
	zero.up.maxRedirects = 0
	resp, _ = zero.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if resp.PassLocation == "" {
		t.Fatal("beyond the hop limit, it should be passed to the client")
	}
}

func TestChallengeAfterRedirect(t *testing.T) {
	// Simulates registry.k8s.io: the manifest first redirects with 307 to pkg.test, which is where a token is
	// required
	e := newEnv(t, `    credentials:
      username: bot
      password: {env: TEST_PW}
      realm_host: auth.test
      public_only: true
`)
	e.reg.ServeManifestHost(t, e.net, "pkg.test")
	resp, body := e.fetch(t, http.MethodGet, "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, e.img.Index) {
		t.Fatalf("should fetch a token for pkg.test's challenge after the redirect: %d %s", resp.StatusCode, body)
	}
	if len(resp.Hops) != 1 || resp.Hops[0] != "pkg.test" {
		t.Fatalf("hops = %v", resp.Hops)
	}
	if e.reg.AuthCount("auth.test") != 0 {
		t.Fatal("fetching a token while following a redirect must not use the configured account")
	}
	// Only requests to the registry itself use the account
	var layer string
	for d := range e.img.Blobs {
		layer = d
		break
	}
	e.reg.CDNHost = ""
	e.fetch(t, http.MethodGet, "/v2/team/app/blobs/"+layer, nil)
	if e.reg.AuthCount("auth.test") != 1 {
		t.Fatalf("fetching a token for the registry should use the account: %d", e.reg.AuthCount("auth.test"))
	}
}

func TestRealmNotAllowed(t *testing.T) {
	e := newEnv(t, "")
	e.up.realmRe, _ = config.CompileHostPatterns([]string{`other\.test`})
	resp, _ := e.fetch(t, http.MethodGet, "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("should return the upstream's 401 when the token service is not in the allowlist: %d", resp.StatusCode)
	}
	if e.reg.Count("GET auth.test token") != 0 {
		t.Fatal("should not contact a token service outside the allowlist")
	}
}

func TestNoLocation(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Fail["registry.test /v2/team/app/manifests/x"] = testutil.FailWith{Status: 307}
	_, err := e.up.Fetch(context.Background(), Request{Method: http.MethodGet, Path: "/v2/team/app/manifests/x", Header: http.Header{}, Repo: "team/app"})
	if !errors.Is(err, ErrNoLocation) {
		t.Fatalf("a missing Location should be an error: %v", err)
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
			t.Errorf("%s: want %v, got %v", raw, want, got)
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
			t.Errorf("%s: want %v, got %v", s, want, got)
		}
	}
}

func TestDialGuardBlocksLoopback(t *testing.T) {
	// No dial replacement: connecting to this machine with the real dialler should be blocked
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
		t.Fatalf("connecting to this machine should be blocked: %v", err)
	}
}

func TestBreakerOpensAndRelayFailsFast(t *testing.T) {
	n := testutil.NewNet(t) // down.test is not in the network: every dial fails
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
			t.Fatal("an unreachable upstream should be an error")
		}
	}
	_, err = up.Fetch(context.Background(), req)
	var co *CircuitOpenError
	if !errors.As(err, &co) || co.RetryAfter <= 0 || !up.BreakerOpen() {
		t.Fatalf("the breaker should open after 3 consecutive failures: %v", err)
	}
	// Requests the client cancels itself do not count as failures
	other := pool.Get("docker.io")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 10; i++ {
		_, _ = other.Fetch(ctx, req)
	}
	if other.BreakerOpen() {
		t.Fatal("requests cancelled by the client should not open the breaker")
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
	// Private networks not allowed: the dial is blocked
	res, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := NewPool(res.Config, Options{Log: slog.New(slog.DiscardHandler)})
	_, err = pool.Get("lab.example").Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/app/manifests/v1", Header: http.Header{}, Repo: "team/app"})
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("connecting to a private network should be refused by default: %v", err)
	}
	// Allow private networks and trust the CA in tls_ca_file
	res, err = config.Parse([]byte(yaml + "    allow_private_network: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ = NewPool(res.Config, Options{Log: slog.New(slog.DiscardHandler)})
	resp, err := pool.Get("lab.example").Fetch(context.Background(), Request{Method: http.MethodHead, Path: "/v2/team/app/manifests/v1", Header: http.Header{}, Repo: "team/app"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("should be able to pull with allow_private_network: %v %v", resp, err)
	}
	resp.Body.Close()
	// Other upstreams still refuse private networks
	if pool.Get("docker.io").client == pool.Get("lab.example").client {
		t.Fatal("an upstream allowed to reach private networks should use a separate connection pool")
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
			t.Errorf("manifests should use HTTP/2: %s", got)
		}
		want := "HTTP/1.1"
		if h2 {
			want = "HTTP/2.0"
		}
		if got := e.reg.Proto("cdn.test blob"); got != want {
			t.Errorf("with blob_http2=%v layers should use %s: %s", h2, want, got)
		}
	}
}
