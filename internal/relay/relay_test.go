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
	reg  *testutil.Registry // registry.test: tokens on auth.test, blobs on cdn.test
	hub  *testutil.Registry // hubreg.test: simulates Docker Hub (library prefix)
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

// newEnvStore builds a test environment with the given operational data store.
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
	e.app, err = app.New(res, []byte(testConfig+extra), app.Options{
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
		t.Fatalf("%s %s: %v", method, path, err)
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
		t.Fatalf("digest %s, bytes %d", dg, n)
	}
	if e.reg.AuthCount("cdn.test") != 0 {
		t.Fatal("tokens must not be sent to the CDN")
	}
	last := e.logs.last()
	if last.Upstream != "registry.test" || len(last.Hops) != 1 || last.Hops[0] != "cdn.test" || last.Status != 200 || last.Size == 0 {
		t.Fatalf("access log: %+v", last)
	}
}

func TestPullAllInOne(t *testing.T) {
	e := newEnv(t, "")
	// Root path: default upstream, single-segment names get library/
	p := &testutil.Puller{Base: e.srv.URL, Host: "mirror.local"}
	if dg, _, err := p.Pull("alpine", "3.22"); err != nil || dg != e.alp.IndexDigest() {
		t.Fatalf("root path: %s %v", dg, err)
	}
	// Path prefix
	if dg, _, err := p.Pull("registry.test/team/app", "v1"); err != nil || dg != e.img.IndexDigest() {
		t.Fatalf("path prefix: %s %v", dg, err)
	}
	// containerd's ns
	p.Header = http.Header{}
	resp, _ := e.do(t, http.MethodHead, "mirror.local", "/v2/team/app/manifests/v1?ns=registry.test", http.Header{"Accept": {testutil.MediaIndex}})
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Content-Digest") != e.img.IndexDigest() {
		t.Fatalf("ns: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestResponseHeaders(t *testing.T) {
	e := newEnv(t, "")
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", http.Header{"Accept": {testutil.MediaIndex}, "Cookie": {"a=b"}})
	h := resp.Header
	for _, k := range []string{"Set-Cookie", "Docker-Ratelimit-Source", "Ratelimit-Remaining", "Alt-Svc"} {
		if h.Get(k) != "" {
			t.Errorf("must not be passed to the client: %s", k)
		}
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("missing security headers: %v", h)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("a manifest requested by tag must not be cached: %q", h.Get("Cache-Control"))
	}
	if h.Get("Content-Type") != testutil.MediaIndex || h.Get("Docker-Content-Digest") != e.img.IndexDigest() {
		t.Errorf("manifest headers: %v", h)
	}
	l := e.layer()
	resp, _ = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+l, nil)
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Docker-Content-Digest") != l {
		t.Errorf("blob headers: %v", resp.Header)
	}
	// The upstream's Ratelimit headers can be passed on by configuration (only directly forwarded responses
	// carry them, so the manifest cache is turned off here)
	e2 := newEnv(t, "proxy:\n  expose_upstream_ratelimit: true\ncache:\n  max_bytes: 0\n")
	resp, _ = e2.do(t, http.MethodHead, "reg.local", "/v2/team/app/manifests/v1", nil)
	if resp.Header.Get("Ratelimit-Remaining") == "" || resp.Header.Get("Docker-Ratelimit-Source") != "" {
		t.Errorf("expose_upstream_ratelimit: %v", resp.Header)
	}
}

func TestRangeAndBlobHead(t *testing.T) {
	e := newEnv(t, "")
	l := e.layer()
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+l, http.Header{"Range": {"bytes=100-199"}})
	if resp.StatusCode != http.StatusPartialContent || len(body) != 100 || string(body) != string(e.img.Blobs[l][100:200]) {
		t.Fatalf("Range: %d %d", resp.StatusCode, len(body))
	}
	// A HEAD that follows to the CDN becomes a Range 0-0 probe (the CDN's signature is only valid for GET)
	resp, body = e.do(t, http.MethodHead, "reg.local", "/v2/team/app/blobs/"+l, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != strconv.Itoa(256<<10) || len(body) != 0 || resp.Header.Get("Content-Range") != "" {
		t.Fatalf("blob HEAD: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t, "")
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/missing/manifests/v1", nil)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "MANIFEST_UNKNOWN") {
		t.Fatalf("missing repository: %d %s", resp.StatusCode, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/busy"] = testutil.FailWith{Status: 429,
		Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}}, Body: `{"errors":[{"code":"TOOMANYREQUESTS","message":"slow down"}]}`}
	resp, body = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/busy", nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || !strings.Contains(string(body), "TOOMANYREQUESTS") {
		t.Fatalf("429 and Retry-After should be passed to the client: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/html"] = testutil.FailWith{Status: 503,
		Header: http.Header{"Content-Type": {"text/html"}}, Body: "<html>cloudflare</html>"}
	resp, body = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/html", nil)
	if resp.StatusCode != 503 || strings.Contains(string(body), "<html>") || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("an HTML error page should be replaced with OCI JSON: %d %s", resp.StatusCode, body)
	}
	e.reg.Fail["registry.test /v2/team/app/manifests/private"] = testutil.FailWith{Status: 401,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: `{"errors":[{"code":"UNAUTHORIZED"}]}`}
	resp, _ = e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/private", nil)
	if resp.StatusCode != 404 || resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("a final upstream 401 should become a 404 without a challenge: %d %v", resp.StatusCode, resp.Header)
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
			t.Errorf("%s %s %s: want %d, got %d %s", c.method, c.host, c.path, c.status, resp.StatusCode, body)
		}
		if c.status == 405 && resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("405 should carry Allow")
		}
	}
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/", nil)
	if string(body) != "{}" || resp.Header.Get("Docker-Distribution-Api-Version") != "registry/2.0" || resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("/v2/ should be answered locally without a challenge: %s %v", body, resp.Header)
	}
}

func TestUnlistedRedirectPassedToClient(t *testing.T) {
	// Remove cdn.test from the allowlist
	e := newEnv(t, "")
	e.cfg.Upstreams["registry.test"].RedirectHosts = []string{`pkg\.test`}
	if err := e.app.Rebuild(); err != nil {
		t.Fatal(err)
	}
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil)
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(resp.Header.Get("Location"), "https://cdn.test/") {
		t.Fatalf("a redirect outside the allowlist should be passed to the client: %d %v", resp.StatusCode, resp.Header)
	}
	if e.app.Stats.Unlisted()["registry.test"]["cdn.test"] != 1 {
		t.Fatalf("the host outside the allowlist should be recorded: %v", e.app.Stats.Unlisted())
	}
}

func TestClientIPInLog(t *testing.T) {
	e := newEnv(t, "")
	e.do(t, http.MethodGet, "reg.local", "/v2/", http.Header{"X-Forwarded-For": {"10.0.0.9, 198.51.100.23"}, "User-Agent": {"containerd/2.1"}})
	last := e.logs.last()
	if last.ClientIP != "198.51.100.23" || last.RemoteIP != "127.0.0.1" || last.UserAgent != "containerd/2.1" || last.Entrance != "reg.local" {
		t.Fatalf("access log: %+v", last)
	}
	var m map[string]any
	if err := json.Unmarshal(accesslog.Marshal(&last), &m); err != nil || m["status"].(float64) != 200 {
		t.Fatalf("log JSON: %v %v", m, err)
	}
}

func TestTagsLinkRewrite(t *testing.T) {
	e := newEnv(t, "")
	resp, body := e.do(t, http.MethodGet, "mirror.local", "/v2/registry.test/team/app/tags/list?n=1", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"v1"`) {
		t.Fatalf("tags: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Link"); got != `</v2/registry.test/team/app/tags/list?last=v1&n=1>; rel="next"` {
		t.Fatalf("Link: %q", got)
	}
}

// The upstream stops sending data midway: give up after upstream_read_idle and abort the connection, so the
// client sees a truncated response rather than a complete one
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
		t.Fatalf("should give up after the idle timeout: %v %v %q", err, time.Since(start), rec.Body.String())
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
		t.Fatalf("the second pull should not GET the manifest from the upstream again: before %d, after %d", gets, got)
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
		t.Fatalf("the index and both platform manifests of the second pull should hit the cache: %d", hits)
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
	// Same Accept as the pulling client: the tag cache key includes Accept
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", http.Header{"Accept": {testutil.MediaIndex + ", " + testutil.MediaManifest}})
	if resp.StatusCode != 200 || string(body) != string(e.img.Index) || e.logs.last().Cache != "stale" {
		t.Fatalf("should return the stale manifest when the upstream returns 503: %d %q", resp.StatusCode, e.logs.last().Cache)
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
		t.Fatalf("no password should return a Basic challenge: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "wrong")); resp.StatusCode != 401 {
		t.Fatalf("a wrong password should return 401: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "correct horse")); resp.StatusCode != 200 {
		t.Fatalf("the correct password should return 200: %d", resp.StatusCode)
	}
	p := &testutil.Puller{Base: e.srv.URL, Host: "private.local", Header: basic("alice", "correct horse")}
	if _, _, err := p.Pull("team/app", "v1"); err != nil {
		t.Fatalf("should be able to pull after signing in: %v", err)
	}
	if e.logs.last().UserID != "alice" {
		t.Fatalf("the access log should record the user: %+v", e.logs.last())
	}
	// By default anyone can view the usage page and only pulls need sign-in; site.json tells the page that
	// sign-in is required here
	if resp, body := e.do(t, http.MethodGet, "private.local", "/", nil); resp.StatusCode != 200 || !strings.Contains(string(body), "<html") {
		t.Fatalf("page defaults to public, so the usage page should show without sign-in: %d", resp.StatusCode)
	}
	if resp, body := e.do(t, http.MethodGet, "private.local", "/site.json", nil); resp.StatusCode != 200 || !strings.Contains(string(body), `"login": true`) {
		t.Fatalf("site.json of an entrance that requires sign-in should carry login: true: %d %s", resp.StatusCode, body)
	}
	// page: login: the usage page needs sign-in too
	if resp, _ := e.do(t, http.MethodGet, "hidden.local", "/", nil); resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic ") {
		t.Fatalf("with page: login, no sign-in should return a Basic challenge: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodGet, "hidden.local", "/status.json", nil); resp.StatusCode != 401 {
		t.Fatalf("with page: login, status.json needs sign-in too: %d", resp.StatusCode)
	}
	if resp, body := e.do(t, http.MethodGet, "hidden.local", "/", basic("alice", "correct horse")); resp.StatusCode != 200 || !strings.Contains(string(body), "<html") {
		t.Fatalf("with page: login, the usage page should show after sign-in: %d", resp.StatusCode)
	}
	// page: off: no usage page, pulls work as usual
	for _, path := range []string{"/", "/site.json", "/status.json"} {
		if resp, _ := e.do(t, http.MethodGet, "bare.local", path, nil); resp.StatusCode != 404 {
			t.Fatalf("with page: off, %s should return 404: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, http.MethodGet, "bare.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatalf("page: off should not affect pulls: %d", resp.StatusCode)
	}
	// Public entrances are not affected
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatal("a public entrance needs no sign-in")
	}
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/", nil); resp.StatusCode != 200 {
		t.Fatal("the usage page of a public entrance needs no sign-in")
	}
	// Disable the account
	e.update(t, func(d *store.Data) { d.Users[0].Disabled = true })
	if resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "correct horse")); resp.StatusCode != 401 {
		t.Fatalf("a disabled account should return 401: %d", resp.StatusCode)
	}
	// Failed attempt limit (10 per minute by default). Use an existing account with a wrong password: its hash
	// cost is low and fast even under -race, so the test is not so slow that tokens refill meanwhile and hide
	// the limit
	e.update(t, func(d *store.Data) { d.Users[0].Disabled = false })
	var last int
	for i := 0; i < 12; i++ {
		resp, _ := e.do(t, http.MethodGet, "private.local", "/v2/", basic("alice", "wrong"))
		last = resp.StatusCode
	}
	if last != 429 {
		t.Fatalf("too many failed sign-ins should return 429: %d", last)
	}
}

func TestIPBan(t *testing.T) {
	e, _ := boltEnv(t, "")
	e.update(t, func(d *store.Data) {
		d.Bans = append(d.Bans, store.Ban{CIDR: "127.0.0.1/32", Until: time.Now().Add(time.Hour), Reason: "test"})
	})
	for _, path := range []string{"/v2/", "/"} {
		if resp, _ := e.do(t, http.MethodGet, "reg.local", path, nil); resp.StatusCode != 403 {
			t.Fatalf("a banned IP requesting %s should get 403: %d", path, resp.StatusCode)
		}
	}
	if e.logs.last().Error != "banned" {
		t.Fatalf("the log should record the reason: %+v", e.logs.last())
	}
	e.update(t, func(d *store.Data) { d.Bans = nil })
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatal("access should be restored after the ban is lifted")
	}
}

func TestImageDenyRule(t *testing.T) {
	e := newEnv(t, `access:
  images:
    deny: [{pattern: 'registry.test/team/**', status: 451, message: blocked}]
`)
	resp, body := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/manifests/v1", nil)
	if resp.StatusCode != 451 || !strings.Contains(string(body), "blocked") || !strings.Contains(string(body), "DENIED") {
		t.Fatalf("a deny-listed image should return 451: %d %s", resp.StatusCode, body)
	}
	if e.reg.Count("GET registry.test manifest") != 0 {
		t.Fatal("a denied request should not be sent to the upstream")
	}
	if resp, _ := e.do(t, http.MethodGet, "mirror.local", "/v2/alpine/manifests/3.22", nil); resp.StatusCode != 200 {
		t.Fatalf("other images should not be affected: %d", resp.StatusCode)
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
			t.Fatal("429 should carry Retry-After")
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || !e.logs.last().Limited {
		t.Fatalf("should return 429 after a burst of 2: %v", codes)
	}
}

func TestBlobStreamLimit(t *testing.T) {
	e := newEnv(t, `limits:
  blob_streams_per_client: 1
`)
	// Take this client's only slot first, simulating a layer still downloading
	rel, ok, _ := e.app.Limiter.AcquireBlob("ip:127.0.0.1", netip.MustParseAddr("127.0.0.1"), limitOverrideNone)
	if !ok {
		t.Fatal("should be able to take a slot")
	}
	resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil)
	if resp.StatusCode != 429 {
		t.Fatalf("exceeding the per-client concurrent streams should return 429: %d", resp.StatusCode)
	}
	rel()
	if resp, _ := e.do(t, http.MethodGet, "reg.local", "/v2/team/app/blobs/"+e.layer(), nil); resp.StatusCode != 200 {
		t.Fatalf("should be able to download after release: %d", resp.StatusCode)
	}
}

var limitOverrideNone = limit.Override{}
