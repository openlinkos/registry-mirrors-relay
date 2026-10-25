package admin

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/api"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
)

const (
	alicePW  = "alice-password-123"
	victorPW = "victor-password-123"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	app    *app.App
	h      *Handler
	mu     sync.Mutex
	clock  time.Time
	origin string
	host   string // used as the request's Host when not empty
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

type envOpts struct {
	yaml  string
	store store.Store
	noAdm bool
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	raw := []byte("log: {access: {output: \"off\"}}\nproxy: {health_interval: 0s}\n" + o.yaml)
	res, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	st := o.store
	if st == nil {
		bs, err := store.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		st = bs
		if !o.noAdm {
			hash := func(p string) string {
				h, _ := bcrypt.GenerateFromPassword([]byte(p), bcrypt.MinCost)
				return string(h)
			}
			if err := st.Update(func(d *store.Data) error {
				d.Admins = []store.Admin{
					{Name: "alice", Hash: hash(alicePW), Role: "admin", Created: time.Now()},
					{Name: "victor", Hash: hash(victorPW), Role: "viewer", Created: time.Now()},
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a, err := app.New(res, raw, app.Options{Log: slog.New(slog.DiscardHandler), Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e := &env{t: t, app: a, clock: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	op := ops.New(a)
	h, err := New(a, op, Options{Log: slog.New(slog.DiscardHandler), Now: e.now, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", api.New(a, op, api.Options{Session: h.Session}))
	mux.Handle("/admin/", h)
	mux.Handle("/admin", h)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.origin = e.srv.URL
	return e
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type resp struct {
	*http.Response
	body string
}

func (r resp) cookie(name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// do sends a request. cookies is a list of name=value pairs; POSTs carry the correct Origin and form
// content type by default.
func (e *env) do(method, path string, form url.Values, cookies []string, hdr map[string]string) resp {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.host != "" {
		req.Host = e.host
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.origin)
	}
	for _, c := range cookies {
		req.Header.Add("Cookie", c)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	r, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r, string(b)}
}

func (e *env) get(path string, cookies ...string) resp { return e.do("GET", path, nil, cookies, nil) }

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

type testSession struct {
	cookie string // name=value
	csrf   string
}

// login logs in and extracts the session cookie and the CSRF token.
func (e *env) login(user, pw string) testSession {
	e.t.Helper()
	r := e.do("POST", "/admin/login", url.Values{"username": {user}, "password": {pw}, "next": {"/admin/"}}, nil, nil)
	if r.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login %s: %d %s", user, r.StatusCode, r.body)
	}
	c := r.Cookies()[0]
	s := testSession{cookie: c.Name + "=" + c.Value}
	page := e.get("/admin/", s.cookie)
	m := csrfRe.FindStringSubmatch(page.body)
	if m == nil {
		e.t.Fatalf("no CSRF token in the page: %d %s", page.StatusCode, page.body)
	}
	s.csrf = m[1]
	return s
}

func (e *env) post(s testSession, path string, form url.Values) resp {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("_csrf", s.csrf)
	return e.do("POST", path, form, []string{s.cookie}, nil)
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.get("/admin/login")
	if r.StatusCode != 200 {
		t.Fatalf("login page: %d", r.StatusCode)
	}
	want := map[string]string{
		"Content-Security-Policy":      "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; require-trusted-types-for 'script'; trusted-types 'none'",
		"Cache-Control":                "no-store, private",
		"Vary":                         "Cookie",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "same-origin",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), bluetooth=(), hid=(), midi=()",
		"X-Robots-Tag":                 "noindex, nofollow",
		"Content-Type":                 "text/html; charset=utf-8",
	}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if r.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent without an https public_url")
	}
	// The root path and /admin
	if r := e.get("/admin"); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/" {
		t.Errorf("/admin should redirect to /admin/: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.get("/admin/"); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login?next=%2Fadmin%2F" {
		t.Errorf("not logged in should redirect to the login page: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.do("PUT", "/admin/", nil, nil, nil); r.StatusCode != 405 || r.Header.Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("only GET, HEAD and POST are allowed: %d %s", r.StatusCode, r.Header.Get("Allow"))
	}
	if r := e.get("/admin/nope"); r.StatusCode != 404 {
		t.Errorf("page that does not exist: %d", r.StatusCode)
	}
}

func TestHostCheck(t *testing.T) {
	// Default admin.public: any domain can open the console; an https page through a TLS-terminating
	// reverse proxy can log in, and the session cookie is Secure
	pub := newEnv(t, envOpts{})
	pub.host, pub.origin = "mirror.example.test", "https://mirror.example.test"
	if r := pub.get("/admin/login"); r.StatusCode != 200 {
		t.Fatalf("with admin.public any domain can open the console: %d", r.StatusCode)
	}
	r := pub.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil)
	if c := r.cookie(secureCookie); r.StatusCode != http.StatusSeeOther || c == nil || !c.Secure {
		t.Fatalf("login from an https page uses the __Host- cookie: %d %+v", r.StatusCode, r.Cookies())
	}
	pub.origin = "http://mirror.example.test"
	if r := pub.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != http.StatusSeeOther || r.cookie(plainCookie) == nil {
		t.Fatalf("a plain http page can log in too (plain cookie): %d", r.StatusCode)
	}
	for _, o := range []string{"https://evil.example.test", "https://mirror.example.test.evil.example", "null"} {
		pub.origin = o
		if r := pub.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %s is not this site: %d", o, r.StatusCode)
		}
	}

	// admin.public: false: only public_url, localhost and IP addresses are accepted, against DNS rebinding
	e := newEnv(t, envOpts{yaml: "admin: {public: false}\n"})
	e.host = "admin.evil.example"
	if r := e.get("/admin/login"); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("without public_url only loopback addresses are accepted: %d", r.StatusCode)
	}
	if r := e.get(api.Prefix + "/status"); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("the API too: %d", r.StatusCode)
	}
	e.host = "localhost"
	if r := e.get("/admin/login"); r.StatusCode != 200 {
		t.Fatalf("localhost is allowed: %d", r.StatusCode)
	}
	// Opening directly by IP on a LAN: no DNS rebinding problem
	for _, h := range []string{"192.0.2.10:8080", "[2001:db8::10]:8080"} {
		e.host = h
		if r := e.get("/admin/login"); r.StatusCode != 200 {
			t.Fatalf("IP %s is allowed: %d", h, r.StatusCode)
		}
	}
	// When opened by IP, Origin is http://<IP> and login works
	e.host, e.origin = "192.0.2.10:8080", "http://192.0.2.10:8080"
	if r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("the console opened by IP should allow login: %d", r.StatusCode)
	}
}

func TestLoginAndCookieFlags(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {"wrong-password-1"}}, nil, nil)
	if r.StatusCode != 401 || !strings.Contains(r.body, tr(i18n.EN, "auth.bad")) || len(r.Cookies()) != 0 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	r = e.do("POST", "/admin/login", url.Values{"username": {"nobody"}, "password": {"whatever-123456"}}, nil, nil)
	if r.StatusCode != 401 {
		t.Fatalf("an unknown user is also 401: %d", r.StatusCode)
	}
	r = e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}, "next": {"/admin/rules?x=1"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/rules?x=1" {
		t.Fatalf("after login, redirect to next: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	c := r.cookie(plainCookie)
	if c == nil || !c.HttpOnly || c.Secure || c.Path != "/admin" || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 0 {
		t.Fatalf("session cookie on a loopback address: %+v", c)
	}
	if !strings.Contains(r.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("Set-Cookie: %s", r.Header.Get("Set-Cookie"))
	}
	page := e.get("/admin/", plainCookie+"="+c.Value)
	if page.StatusCode != 200 || !strings.Contains(page.body, "alice") {
		t.Fatalf("the overview is visible after login: %d", page.StatusCode)
	}
	// Open redirect
	r = e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}, "next": {"//evil.example/"}}, nil, nil)
	if r.Header.Get("Location") != "/admin/" {
		t.Fatalf("next must not point to another site: %s", r.Header.Get("Location"))
	}
	// Logout
	s := testSession{cookie: plainCookie + "=" + c.Value}
	s.csrf = csrfRe.FindStringSubmatch(page.body)[1]
	if r := e.post(s, "/admin/logout", nil); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := e.get("/admin/", s.cookie); r.StatusCode != 303 {
		t.Fatalf("the session is invalid after logout: %d", r.StatusCode)
	}
	log := e.app.Store.AuditLog(100)
	var actions []string
	for _, a := range log {
		actions = append(actions, a.Action)
	}
	if strings.Count(strings.Join(actions, ","), "login.failed") != 1 || !slices.Contains(actions, "login") || !slices.Contains(actions, "logout") {
		t.Fatalf("audit log: only the first failure is recorded, login and logout both are: %v", actions)
	}
}

func TestSecureCookieBehindHTTPS(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "admin: {public_url: \"https://admin.example.test\"}\n"})
	e.host, e.origin = "admin.example.test", "https://admin.example.test"
	r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil)
	c := r.cookie(secureCookie)
	if r.StatusCode != 303 || c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("https uses the __Host- cookie: %d %+v", r.StatusCode, r.Cookies())
	}
	if r.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatal("an https public_url must send HSTS")
	}
	if r := e.get("/admin/", secureCookie+"="+c.Value); r.StatusCode != 200 {
		t.Fatalf("the __Host- cookie gives access: %d", r.StatusCode)
	}
	// The same session ID in a plain cookie is not accepted (a sibling subdomain can plant plain cookies)
	if r := e.get("/admin/", plainCookie+"="+c.Value); r.StatusCode != 303 {
		t.Fatalf("a plain cookie is not accepted under https: %d", r.StatusCode)
	}
	// An http origin is not accepted
	e.origin = "http://admin.example.test"
	if r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != 403 {
		t.Fatalf("the origin must match public_url exactly: %d", r.StatusCode)
	}
}

func TestPostChecks(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	form := func() url.Values {
		return url.Values{"_csrf": {s.csrf}, "cidr": {"198.51.100.7"}, "duration": {"1h"}}
	}
	// Change the first character; if it already is x, use y, otherwise the result is the same token
	wrong := "x" + s.csrf[1:]
	if wrong == s.csrf {
		wrong = "y" + s.csrf[1:]
	}
	cases := []struct {
		name string
		hdr  map[string]string
		form url.Values
		want int
	}{
		{"no Origin", map[string]string{"Origin": ""}, form(), 403},
		{"foreign Origin", map[string]string{"Origin": "http://evil.example"}, form(), 403},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, form(), 403},
		{"no CSRF", nil, url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}, 403},
		{"wrong CSRF", nil, url.Values{"_csrf": {wrong}, "cidr": {"198.51.100.7"}, "duration": {"1h"}}, 403},
		{"not a form", map[string]string{"Content-Type": "application/json"}, form(), 415},
	}
	for _, c := range cases {
		if r := e.do("POST", "/admin/bans/add", c.form, []string{s.cookie}, c.hdr); r.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.StatusCode, c.want)
		}
	}
	if len(e.app.Runtime().Data.Bans) != 0 {
		t.Fatal("rejected requests must not take effect")
	}
	r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}, "reason": {"test"}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/rules?ok=ban.added&v=198.51.100.7%2F32" {
		t.Fatalf("ban: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if page := e.get(r.Header.Get("Location"), s.cookie); !strings.Contains(page.body, "Banned 198.51.100.7/32.") {
		t.Fatal("the success notice should show after the redirect")
	}
	// Read-only role
	v := e.login("victor", victorPW)
	if r := e.post(v, "/admin/bans/delete", url.Values{"cidr": {"198.51.100.7"}}); r.StatusCode != 403 {
		t.Fatalf("the read-only role cannot modify: %d", r.StatusCode)
	}
	// A POST without login redirects to the login page
	if r := e.do("POST", "/admin/bans/add", form(), nil, nil); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" {
		t.Fatalf("not logged in: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// Form errors: re-rendered with the explanation
	r = e.post(s, "/admin/bans/add", url.Values{"cidr": {"0.0.0.0/0"}, "duration": {"1h"}})
	if r.StatusCode != 400 || !strings.Contains(r.body, "A rule cannot cover every address.") || !strings.Contains(r.body, `value="0.0.0.0/0"`) {
		t.Fatalf("an error should re-render and keep the filled-in values: %d", r.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, envOpts{})
	try := func(pw string) resp {
		return e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {pw}}, nil, nil)
	}
	for i := 1; i <= 10; i++ {
		if r := try("wrong-password-1"); r.StatusCode != 401 {
			t.Fatalf("failure %d should return 401: %d", i, r.StatusCode)
		}
	}
	r := try(alicePW)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" || len(r.Cookies()) != 0 {
		t.Fatalf("after 10 failures even the right password is rejected: %d %s", r.StatusCode, r.Header.Get("Retry-After"))
	}
	try(alicePW)
	e.advance(10*time.Minute + time.Second)
	if r := try(alicePW); r.StatusCode != 303 {
		t.Fatalf("login works once the window has passed: %d", r.StatusCode)
	}
	var failed, blocked int
	for _, a := range e.app.Store.AuditLog(100) {
		switch a.Action {
		case "login.failed":
			failed++
		case "login.blocked":
			blocked++
		}
	}
	if failed != 1 || blocked != 1 {
		t.Fatalf("one failure and one block are recorded per window: failed=%d blocked=%d", failed, blocked)
	}
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	e.advance(31 * time.Minute)
	if r := e.get("/admin/rules", s.cookie); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login?next=%2Fadmin%2Frules" {
		t.Fatalf("log in again after 30 minutes idle: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	s = e.login("alice", alicePW)
	start := e.now()
	for e.now().Sub(start) < 12*time.Hour-29*time.Minute {
		e.advance(29 * time.Minute)
		if r := e.get("/admin/", s.cookie); r.StatusCode != 200 {
			t.Fatalf("valid for 12 hours with continuous activity (%s): %d", e.now().Sub(start), r.StatusCode)
		}
	}
	e.advance(29 * time.Minute)
	if r := e.get("/admin/", s.cookie); r.StatusCode != 303 {
		t.Fatalf("invalid 12 hours after login (%s): %d", e.now().Sub(start), r.StatusCode)
	}
}

func TestFreshForSensitiveActions(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	r := e.post(s, "/admin/tokens/create", url.Values{"name": {"ci"}, "role": {"viewer"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, "rmr_") {
		t.Fatalf("a token can be created right after login: %d", r.StatusCode)
	}
	e.advance(11 * time.Minute)
	r = e.post(s, "/admin/tokens/create", url.Values{"name": {"ci2"}, "role": {"viewer"}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/reauth?next=%2Fadmin%2Fadmins" {
		t.Fatalf("re-authentication is required after 10 minutes: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.get("/admin/reauth?next=%2Fadmin%2Fadmins", s.cookie); r.StatusCode != 200 {
		t.Fatalf("re-authentication page: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/reauth", url.Values{"password": {"wrong-password-1"}, "next": {"/admin/admins"}}); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/reauth", url.Values{"password": {alicePW}, "next": {"/admin/admins"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/admins" {
		t.Fatalf("back to the original page after re-authentication: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.post(s, "/admin/tokens/create", url.Values{"name": {"ci2"}, "role": {"viewer"}}); r.StatusCode != 200 {
		t.Fatalf("can create after re-authentication: %d", r.StatusCode)
	}
}

func TestWriteRoutesPinned(t *testing.T) {
	e := newEnv(t, envOpts{})
	want := []string{
		"/admin/account/password", "/admin/admins/create", "/admin/admins/delete", "/admin/admins/update",
		"/admin/bans/add", "/admin/bans/delete", "/admin/config/reset", "/admin/config/save",
		"/admin/entrances/delete", "/admin/entrances/save", "/admin/limits/save", "/admin/login", "/admin/logout",
		"/admin/prefs", "/admin/reauth", "/admin/reload", "/admin/rules/image/add", "/admin/rules/image/delete",
		"/admin/rules/ip/add", "/admin/rules/ip/delete", "/admin/sessions/revoke", "/admin/site/reset",
		"/admin/site/save", "/admin/site/status", "/admin/tokens/create", "/admin/tokens/delete",
		"/admin/upstreams/check", "/admin/upstreams/hosts/add", "/admin/upstreams/hosts/delete",
		"/admin/upstreams/save", "/admin/users/create", "/admin/users/delete", "/admin/users/toggle",
		"/admin/users/update",
	}
	if got := e.h.WriteRoutes(); !slices.Equal(got, want) {
		t.Fatalf("the console's writes changed:\n%v", got)
	}
	// Sensitive operations all require a recently entered password
	for _, r := range e.h.routes {
		// Configuration changes, wherever they are made, as well as administrators and tokens
		sensitive := strings.HasPrefix(r.path, "/admin/admins/") || strings.HasPrefix(r.path, "/admin/tokens/") ||
			strings.HasPrefix(r.path, "/admin/config/") || strings.HasPrefix(r.path, "/admin/entrances/") ||
			slices.Contains([]string{"/admin/sessions/revoke", "/admin/account/password", "/admin/upstreams/save",
				"/admin/limits/save", "/admin/site/status"}, r.path)
		if r.method == http.MethodPost && sensitive && r.auth != authFresh {
			t.Errorf("%s should require re-authentication", r.path)
		}
	}
}

// fillStats creates some request records so that every page has content.
func fillStats(a *app.App) {
	for i := range 6 {
		a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "198.51.100.9", Method: "GET", Host: "mirror.test",
			URI: "/v2/library/nginx/blobs/sha256:abc", Status: 200, Size: 1 << 20, RespContentLength: "1048576",
			Duration: time.Duration(i+1) * time.Millisecond, Upstream: "docker.io", Image: "docker.io/library/nginx",
			Kind: "blob", Ref: "sha256:abc", UserID: "bob"})
	}
	a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "203.0.113.5", Method: "GET", Host: "mirror.test",
		URI: "/v2/library/redis/manifests/7", Status: 429, Limited: true, Upstream: "docker.io", Image: "docker.io/library/redis", Kind: "manifest"})
	a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "203.0.113.6", Method: "GET", Host: "mirror.test",
		URI: "/v2/x/manifests/1", Status: 502, Error: "<script>alert(1)</script>", Upstream: "ghcr.io", Image: "ghcr.io/x", Kind: "manifest"})
}

var (
	inlineStyle  = regexp.MustCompile(`(?i)\sstyle=`)
	inlineScript = regexp.MustCompile(`(?i)<script(\s[^>]*)?>`)
	eventAttr    = regexp.MustCompile(`(?i)\son[a-z]+=`)
	iconUse      = regexp.MustCompile(`<use href="#i-([a-z0-9-]+)"/>`)
)

// checkPage is what every page must satisfy: no inline styles, inline scripts or event attributes; every
// icon used has a symbol; no untranslated text.
func checkPage(t *testing.T, name, body string) {
	t.Helper()
	if inlineStyle.MatchString(body) {
		t.Errorf("%s has inline styles", name)
	}
	for _, m := range inlineScript.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(m[1], "src=") {
			t.Errorf("%s has an inline script", name)
		}
	}
	if eventAttr.MatchString(body) {
		t.Errorf("%s has event attributes", name)
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Errorf("%s is not escaped", name)
	}
	for _, m := range iconUse.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `<symbol id="i-`+m[1]+`"`) {
			t.Errorf("%s uses icon %s but has no symbol", name, m[1])
		}
	}
	if strings.Contains(body, ui.SpriteMark) {
		t.Errorf("%s: the icon placeholder was not replaced", name)
	}
	for k := range messagesZH {
		if strings.Contains(body, ">"+k+"<") || strings.Contains(body, `"`+k+`"`) {
			t.Errorf("%s has untranslated key %s", name, k)
		}
	}
}

func TestPagesRender(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "entrances:\n  - {name: mirror, hosts: [mirror.test]}\n  - {hosts: [ghcr.test], upstream: ghcr.io, auth: basic}\n"})
	admin := e.login("alice", alicePW)
	if r := e.post(admin, "/admin/users/create", url.Values{"name": {"bob"}, "password": {"bob-password-123"}, "manifest_rate": {"5"}}); r.StatusCode != 200 || strings.Contains(r.body, `id="secret-value"`) {
		t.Fatalf("create account: %d %s", r.StatusCode, r.body)
	}
	e.post(admin, "/admin/rules/image/add", url.Values{"pattern": {"ghcr.io/evil/**"}, "kind": {"deny"}, "status": {"451"}})
	e.app.Stats.UnlistedRedirect("quay.io", "cdn99.quay.io")
	fillStats(e.app)
	viewer := e.login("victor", victorPW)
	pages := []string{
		"/admin/", "/admin/upstreams", "/admin/upstreams/edit?name=ghcr.io", "/admin/upstreams/edit",
		"/admin/entrances", "/admin/entrances/view?i=0", "/admin/entrances/view?i=0&host=mirror.test&scheme=http",
		"/admin/entrances/view?i=1", "/admin/entrances/view?new=1", "/admin/site", "/admin/users",
		"/admin/users?q=bo&status=active", "/admin/users?q=nobody", "/admin/users/new",
		"/admin/users/edit?name=bob", "/admin/rules", "/admin/rules?image=ghcr.io/evil/miner:1", "/admin/rules?image=nginx",
		"/admin/limits", "/admin/requests", "/admin/requests?status=5&q=x", "/admin/requests?user=bob", "/admin/requests?page=3",
		"/admin/abuse", "/admin/abuse?hours=168", "/admin/audit", "/admin/admins", "/admin/admins/edit?name=victor",
		"/admin/system", "/admin/account", "/admin/reauth", "/admin/config", "/admin/nope",
	}
	for _, lang := range []string{"zh", "en"} {
		for _, who := range []testSession{admin, viewer} {
			for _, p := range pages {
				r := e.get(p, who.cookie, langCookie+"="+lang)
				want := 200
				switch {
				case p == "/admin/nope":
					want = 404
				case who == viewer && (p == "/admin/users/new" || p == "/admin/entrances/view?new=1"):
					want = 403 // pages that only create something
				}
				if r.StatusCode != want {
					t.Fatalf("%s (%s): %d %s", p, lang, r.StatusCode, r.body)
				}
				checkPage(t, p+" "+lang, r.body)
				if lang == "en" && !strings.Contains(r.body, `<html lang="en"`) {
					t.Errorf("%s should be in English", p)
				}
			}
		}
	}
	for _, p := range []string{"/admin/login", "/admin/login?next=/admin/rules"} {
		r := e.get(p)
		checkPage(t, p, r.body)
	}
	// The read-only role does not see the edit forms
	if r := e.get("/admin/users", viewer.cookie); strings.Contains(r.body, `href="/admin/users/new"`) || strings.Contains(r.body, `action="/admin/users/toggle"`) {
		t.Error("the read-only role should not see the create button or the account actions")
	}
	if r := e.get("/admin/users", admin.cookie); !strings.Contains(r.body, `href="/admin/users/new"`) || !strings.Contains(r.body, `action="/admin/users/toggle"`) {
		t.Error("an administrator should see the create button and the account actions")
	}
	if r := e.get("/admin/users/new", admin.cookie); !strings.Contains(r.body, `action="/admin/users/create"`) {
		t.Error("the new account page should have the create form")
	}
	// Addresses of earlier versions lead to their new places
	for old, want := range map[string]string{
		"/admin/usage": "/admin/entrances", "/admin/settings": "/admin/site", "/admin/config/entrances": "/admin/entrances",
		"/admin/config/upstream?name=ghcr.io": "/admin/upstreams/edit?name=ghcr.io",
	} {
		if r := e.get(old, admin.cookie); r.StatusCode != 303 || r.Header.Get("Location") != want {
			t.Errorf("%s: %d %s, want %s", old, r.StatusCode, r.Header.Get("Location"), want)
		}
	}
	// Live numbers on the overview
	r := e.get("/admin/live", admin.cookie)
	var live map[string]string
	if r.StatusCode != 200 || json.Unmarshal([]byte(r.body), &live) != nil || live["rpm"] == "" {
		t.Fatalf("/admin/live: %d %s", r.StatusCode, r.body)
	}
	if r := e.do("GET", "/admin/live", nil, []string{admin.cookie}, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("/admin/live does not accept cross-site requests: %d", r.StatusCode)
	}
}

func TestMessagesComplete(t *testing.T) {
	for k := range messagesZH {
		if _, ok := messagesEN[k]; !ok {
			t.Errorf("English is missing %s", k)
		}
	}
	for k := range messagesEN {
		if _, ok := messagesZH[k]; !ok {
			t.Errorf("Chinese is missing %s", k)
		}
	}
	// Every key used in the templates and the code must exist
	used := regexp.MustCompile(`\.T "([a-zA-Z0-9_.]+)"|tr\((?:c\.)?lang, "([a-zA-Z0-9_.]+)"|fail\(http\.\w+, "([a-zA-Z0-9_.]+)"|message: "([a-zA-Z0-9_.]+)"`)
	check := func(name string, b []byte) {
		for _, m := range used.FindAllStringSubmatch(string(b), -1) {
			k := m[1] + m[2] + m[3] + m[4]
			if strings.HasSuffix(k, ".") {
				continue // a composed key, checked separately below
			}
			if _, ok := messagesZH[k]; !ok {
				t.Errorf("%s uses missing key %s", name, k)
			}
		}
	}
	_ = fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			b, _ := templateFS.ReadFile(p)
			check(p, b)
		}
		return nil
	})
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		check(f, b)
	}
	// Composed keys
	for _, k := range []string{"theme.auto", "theme.light", "theme.dark", "role.admin", "role.viewer", "abuse.hours1", "abuse.hours24", "abuse.hours168"} {
		if _, ok := messagesZH[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	for _, k := range ops.SiteLinkKeys {
		if _, ok := messagesZH["settings.link."+k]; !ok {
			t.Errorf("missing settings.link.%s", k)
		}
	}
	for _, k := range []string{"builtin", "config", "console"} {
		if _, ok := messagesZH["config.source."+k]; !ok {
			t.Errorf("missing config.source.%s", k)
		}
	}
	for _, key := range append(append([]string{}, config.OverrideSections...), "upstreams/x.example.com") {
		sec, _, _ := strings.Cut(key, "/")
		if sec == key {
			for _, k := range []string{"cfg." + key, "cfg." + key + "Help"} {
				if _, ok := messagesZH[k]; !ok {
					t.Errorf("missing %s", k)
				}
			}
		}
		for _, f := range config.OverrideFields(key) {
			if _, ok := messagesZH["cfg."+sec+"."+f]; !ok {
				t.Errorf("missing cfg.%s.%s", sec, f)
			}
		}
	}
	for _, n := range notices {
		if _, ok := messagesZH["notice."+n]; !ok {
			t.Errorf("missing notice.%s", n)
		}
	}
	for _, g := range navTable {
		for _, it := range g.items {
			if _, ok := messagesZH["nav."+it[0]]; !ok {
				t.Errorf("missing nav.%s", it[0])
			}
		}
	}
}

func TestAssets(t *testing.T) {
	e := newEnv(t, envOpts{})
	page := e.get("/admin/login").body
	for _, name := range []string{"ui.css", "admin.css", "admin.js", ui.Mark("", "favicon.svg"), ui.Mark("", "logo-dark.svg")} {
		u := e.h.assetURL(name)
		if !strings.Contains(page, u) {
			t.Errorf("the page should reference %s", u)
		}
		r := e.get(u)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
			t.Fatalf("%s: %d %s", u, r.StatusCode, r.Header.Get("Cache-Control"))
		}
		wantCSP := CSP
		if strings.HasSuffix(name, ".svg") {
			wantCSP = SVGCSP
		}
		if r.Header.Get("Content-Security-Policy") != wantCSP {
			t.Errorf("%s CSP: %s", name, r.Header.Get("Content-Security-Policy"))
		}
	}
	css := e.get(e.h.assetURL("ui.css")).body
	m := regexp.MustCompile(`url\((fonts/Geist-Variable\.[0-9a-f]{10}\.woff2)\)`).FindStringSubmatch(css)
	if m == nil || strings.Contains(css, "url(fonts/Geist-Variable.woff2)") {
		t.Fatal("font URLs in the stylesheet should be rewritten to hashed names")
	}
	if r := e.get("/admin/assets/" + m[1]); r.StatusCode != 200 || r.Header.Get("Content-Type") != "font/woff2" {
		t.Fatalf("font: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := e.get("/admin/assets/ui.css"); r.StatusCode != 404 {
		t.Fatalf("names without a hash are not served: %d", r.StatusCode)
	}
	if r := e.do("POST", e.h.assetURL("admin.css"), url.Values{}, nil, nil); r.StatusCode != 405 {
		t.Fatalf("static files are GET only: %d", r.StatusCode)
	}
}

func TestPrefs(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.do("POST", "/admin/prefs", url.Values{"lang": {"en"}, "theme": {"dark"}, "next": {"/admin/login"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" || r.cookie(langCookie) == nil || r.cookie(themeCookie) == nil {
		t.Fatalf("prefs: %d %v", r.StatusCode, r.Cookies())
	}
	page := e.get("/admin/login", langCookie+"=en", themeCookie+"=dark")
	if !strings.Contains(page.body, `<html lang="en" data-theme="dark">`) || !strings.Contains(page.body, "Sign in") {
		t.Fatal("should be English and dark")
	}
	if r := e.do("POST", "/admin/prefs", url.Values{"lang": {"en"}}, nil, map[string]string{"Origin": "http://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("prefs must check the origin too: %d", r.StatusCode)
	}
	for _, c := range []struct {
		cookie, accept string
		want           i18n.Lang
	}{
		{"", "en-US,en;q=0.9,zh;q=0.8", i18n.EN},
		{"", "fr, zh-CN;q=0.5", i18n.ZH},
		{"", "fr", i18n.EN},
		{langCookie + "=zh", "en", i18n.ZH},
		{oldLangCookie + "=zh", "en", i18n.ZH},
		{langCookie + "=en; " + oldLangCookie + "=zh", "zh", i18n.EN},
	} {
		r := httptest.NewRequest("GET", "/admin/", nil)
		r.Header.Set("Accept-Language", c.accept)
		if c.cookie != "" {
			r.Header.Set("Cookie", c.cookie)
		}
		if got := Lang(r); got != c.want {
			t.Errorf("Lang(cookie %q, Accept-Language %q) = %s, want %s", c.cookie, c.accept, got, c.want)
		}
	}
	if c := r.cookie(langCookie); c.Path != "/" || c.HttpOnly {
		t.Errorf("the language cookie must be readable by the usage page: %+v", c)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/admin/rules?x=1":        "/admin/rules?x=1",
		"/admin":                  "/admin",
		"":                        "/admin/",
		"https://evil.example/":   "/admin/",
		"//evil.example/admin/":   "/admin/",
		"/admin//evil.example":    "/admin/",
		"/admin/\\evil":           "/admin/",
		"/adminx":                 "/admin/",
		"/admin/api/v1/status":    "/admin/",
		"/admin/assets/x.css":     "/admin/",
		"/admin/ok\r\nSet-Cookie": "/admin/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCRUDAndOwnPassword(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	// Without a password one is generated and shown only once
	r := e.post(s, "/admin/users/create", url.Values{"name": {"ci"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, `id="secret-value"`) {
		t.Fatalf("generate password: %d", r.StatusCode)
	}
	pw := regexp.MustCompile(`id="secret-value">([^<]+)<`).FindStringSubmatch(r.body)[1]
	if ok, _ := e.app.Auth.Verify(e.app.Runtime().Users["ci"], pw); !ok {
		t.Fatal("the generated password should work")
	}
	if r := e.post(s, "/admin/users/create", url.Values{"name": {"ci"}, "password": {"another-password"}}); r.StatusCode != 409 {
		t.Fatalf("duplicate name: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/users/update", url.Values{"name": {"ci"}, "part": {"limits"}, "manifest_rate": {"3"}, "note": {"build"}}); r.StatusCode != 303 {
		t.Fatalf("update: %d %s", r.StatusCode, r.body)
	}
	if r := e.post(s, "/admin/users/toggle", url.Values{"name": {"ci"}, "disabled": {"true"}, "next": {"/admin/users"}}); r.StatusCode != 303 ||
		r.Header.Get("Location") != "/admin/users?ok=user.disabled&v=ci" {
		t.Fatalf("disable: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if u := e.app.Runtime().Users["ci"]; !u.Disabled || u.ManifestRPS != 3 {
		t.Fatalf("the update should take effect: %+v", u)
	}
	if r := e.post(s, "/admin/users/toggle", url.Values{"name": {"ci"}, "disabled": {"false"}, "next": {"/admin/users/edit?name=ci"}}); r.StatusCode != 303 ||
		r.Header.Get("Location") != "/admin/users/edit?name=ci&ok=user.enabled&v=ci" || e.app.Runtime().Users["ci"].Disabled {
		t.Fatalf("enable: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// A new password: typed, or generated and shown once; the limits stay
	r = e.post(s, "/admin/users/update", url.Values{"name": {"ci"}, "part": {"password"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, `id="secret-value"`) {
		t.Fatalf("generate a new password: %d", r.StatusCode)
	}
	pw = regexp.MustCompile(`id="secret-value">([^<]+)<`).FindStringSubmatch(r.body)[1]
	if ok, _ := e.app.Auth.Verify(e.app.Runtime().Users["ci"], pw); !ok || e.app.Runtime().Users["ci"].ManifestRPS != 3 {
		t.Fatal("the new password should work and the limits stay")
	}
	if r := e.post(s, "/admin/users/delete", url.Values{"name": {"ci"}}); r.StatusCode != 303 || e.app.Runtime().Users["ci"] != nil {
		t.Fatalf("delete: %d", r.StatusCode)
	}

	// Administrators: cannot delete themselves; changing someone else's role ends that person's sessions
	v := e.login("victor", victorPW)
	if r := e.post(s, "/admin/admins/delete", url.Values{"name": {"alice"}}); r.StatusCode != 403 {
		t.Fatalf("cannot delete oneself: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/update", url.Values{"name": {"victor"}, "role": {"admin"}}); r.StatusCode != 303 {
		t.Fatalf("change role: %d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/", v.cookie); r.StatusCode != 303 {
		t.Fatalf("the old session ends after the role change: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"short"}, "role": {"viewer"}}); r.StatusCode != 400 {
		t.Fatalf("password too short: %d", r.StatusCode)
	}

	// Changing one's own password: the session in another browser ends, and the current browser gets a new
	// session
	other := e.login("alice", alicePW)
	r = e.post(s, "/admin/account/password", url.Values{"password": {"alice-new-password"}, "confirm": {"alice-new-password"}})
	if r.StatusCode != 303 || r.cookie(plainCookie) == nil {
		t.Fatalf("change password: %d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/", other.cookie); r.StatusCode != 303 {
		t.Fatal("other sessions should end")
	}
	if r := e.get("/admin/", plainCookie+"="+r.cookie(plainCookie).Value); r.StatusCode != 200 {
		t.Fatal("the current browser should get a new session")
	}
	e.login("alice", "alice-new-password")
}

func TestAPIWithSession(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	if r := e.get(api.Prefix+"/status", s.cookie); r.StatusCode != 200 {
		t.Fatalf("a session can read the API: %d", r.StatusCode)
	}
	body := `{"cidr":"198.51.100.8","duration":"1h"}`
	call := func(hdr map[string]string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+api.Prefix+"/bans", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", s.cookie)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if c := call(map[string]string{"Origin": e.origin}); c != 403 {
		t.Fatalf("write without X-CSRF-Token: %d", c)
	}
	if c := call(map[string]string{"X-CSRF-Token": s.csrf}); c != 403 {
		t.Fatalf("write without Origin: %d", c)
	}
	if c := call(map[string]string{"X-CSRF-Token": s.csrf, "Origin": e.origin}); c != 201 {
		t.Fatalf("a session plus CSRF can write: %d", c)
	}
	if r := e.get(api.Prefix + "/status"); r.StatusCode != 401 {
		t.Fatalf("neither session nor token: %d", r.StatusCode)
	}
}

func TestReadOnlyStoreWithConfigAdmin(t *testing.T) {
	t.Setenv("T_ADMIN_PW", "config-admin-password")
	e := newEnv(t, envOpts{store: store.NewReadOnly(), yaml: "admin:\n  store: {driver: none}\n  initial_admin: {username: root, password: {env: T_ADMIN_PW}}\n"})
	s := e.login("root", "config-admin-password")
	page := e.get("/admin/users", s.cookie)
	if !strings.Contains(page.body, tr(i18n.EN, "readonly.banner")) || strings.Contains(page.body, `href="/admin/users/new"`) {
		t.Fatal("read-only shows the notice and no forms")
	}
	if r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}); r.StatusCode != 409 || !strings.Contains(r.body, "read-only") {
		t.Fatalf("changes return 409 when read-only: %d", r.StatusCode)
	}
	if r := e.get("/admin/account", s.cookie); !strings.Contains(r.body, tr(i18n.EN, "account.fromConfig")) {
		t.Fatal("an administrator from the configuration file changes the password in the configuration")
	}
}

func TestNoAdmins(t *testing.T) {
	e := newEnv(t, envOpts{noAdm: true})
	if r := e.get("/admin/login"); !strings.Contains(r.body, tr(i18n.EN, "auth.noAdminsTitle")) {
		t.Fatal("with no administrators the login page should explain how to create one")
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                             "0 s",
		45 * time.Second:              "45 s",
		time.Hour:                     "1 h",
		6*time.Hour + 5*time.Minute:   "6 h 5 min",
		50*time.Hour + 30*time.Minute: "2 d 2 h",
	} {
		if got := fmtDur(i18n.EN, d); got != want {
			t.Errorf("fmtDur(%s) = %q, want %q", d, got, want)
		}
	}
	// Chinese uses the Chinese units
	if got, want := fmtDur(i18n.ZH, 90*time.Minute), tr(i18n.ZH, "dur.hours", "n", 1)+" "+tr(i18n.ZH, "dur.minutes", "n", 30); got != want {
		t.Errorf("Chinese: %q, want %q", got, want)
	}
}

func TestInitialAdminMustChangePassword(t *testing.T) {
	t.Setenv("T_INIT_PW", "initial-admin-password")
	e := newEnv(t, envOpts{noAdm: true, yaml: "admin:\n  initial_admin: {username: root, password: {env: T_INIT_PW}}\n"})
	if a := e.app.Store.Data().Admins; len(a) != 1 || !a[0].MustChange {
		t.Fatalf("an administrator created from initial_admin must change the password first: %+v", a)
	}
	r := e.do("POST", "/admin/login", url.Values{"username": {"root"}, "password": {"initial-admin-password"}, "next": {"/admin/rules"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("after login, change the password first: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	c := plainCookie + "=" + r.cookie(plainCookie).Value
	for _, p := range []string{"/admin/", "/admin/rules", "/admin/admins", "/admin/live"} {
		if r := e.get(p, c); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
			t.Errorf("%s must not open before the password is changed: %d %s", p, r.StatusCode, r.Header.Get("Location"))
		}
	}
	page := e.get("/admin/account", c)
	if page.StatusCode != 200 || !strings.Contains(page.body, tr(i18n.EN, "account.mustChangeTitle")) {
		t.Fatalf("the My account page should explain why: %d", page.StatusCode)
	}
	s := testSession{cookie: c, csrf: csrfRe.FindStringSubmatch(page.body)[1]}
	if r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("writes are blocked too: %d", r.StatusCode)
	}
	if r := e.get(api.Prefix+"/status", c); r.StatusCode != 403 {
		t.Fatalf("the API cannot be used with the session either: %d", r.StatusCode)
	}
	// Setting the same password again is not allowed
	if r := e.post(s, "/admin/account/password", url.Values{"password": {"initial-admin-password"}, "confirm": {"initial-admin-password"}}); r.StatusCode != 400 || !strings.Contains(r.body, "The new password must differ from the current one.") {
		t.Fatalf("cannot keep the old password: %d", r.StatusCode)
	}
	r = e.post(s, "/admin/account/password", url.Values{"password": {"root-own-password-1"}, "confirm": {"root-own-password-1"}})
	if r.StatusCode != 303 {
		t.Fatalf("change password: %d %s", r.StatusCode, r.body)
	}
	c = plainCookie + "=" + r.cookie(plainCookie).Value
	if r := e.get("/admin/rules", c); r.StatusCode != 200 {
		t.Fatalf("usable after the password change: %d", r.StatusCode)
	}
	if e.app.Store.Data().Admins[0].MustChange {
		t.Fatal("no longer required after the password change")
	}
	// The old password from the configuration can no longer log in
	if r := e.do("POST", "/admin/login", url.Values{"username": {"root"}, "password": {"initial-admin-password"}}, nil, nil); r.StatusCode != 401 {
		t.Fatalf("the old password should no longer work: %d", r.StatusCode)
	}
}

func TestPasswordsSetByOthersMustChange(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"carol"}, "role": {"viewer"}}); r.StatusCode != 400 {
		t.Fatalf("the password must not equal the user name: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"carol-temp-password"}, "role": {"viewer"}}); r.StatusCode != 303 {
		t.Fatalf("create administrator: %d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/admins", s.cookie); !strings.Contains(r.body, tr(i18n.EN, "admins.mustChange")) {
		t.Fatal("the list should mark the pending password change")
	}
	r := e.do("POST", "/admin/login", url.Values{"username": {"carol"}, "password": {"carol-temp-password"}}, nil, nil)
	if r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("an account created by someone else changes the password at first login: %s", r.Header.Get("Location"))
	}
	// Resetting someone else's password: they must change it first too
	if r := e.post(s, "/admin/admins/update", url.Values{"name": {"victor"}, "role": {"viewer"}, "password": {"victor-reset-password"}}); r.StatusCode != 303 {
		t.Fatalf("reset password: %d", r.StatusCode)
	}
	if r := e.do("POST", "/admin/login", url.Values{"username": {"victor"}, "password": {"victor-reset-password"}}, nil, nil); r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("an administrator whose password was reset changes it first: %s", r.Header.Get("Location"))
	}
	// Changing one's own password does not trigger this
	if r := e.post(s, "/admin/account/password", url.Values{"password": {"alice-second-password"}, "confirm": {"alice-second-password"}}); r.StatusCode != 303 {
		t.Fatalf("change own password: %d", r.StatusCode)
	}
	for _, a := range e.app.Store.Data().Admins {
		if a.Name == "alice" && a.MustChange {
			t.Fatal("a password one set oneself need not be changed again")
		}
	}
}

func TestConfigPages(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "limits: {manifest_rate: 3}\n"})
	s := e.login("alice", alicePW)
	// Limits: only the fields that differ from the configuration file are stored
	form := url.Values{"key": {"limits"}}
	for _, f := range config.OverrideFields("limits") {
		form.Set(f, config.FieldText(e.app.Runtime().Config, "limits", f))
	}
	form.Set("max_inflight", "77")
	if r := e.post(s, "/admin/config/save", form); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/config?ok=config.saved&v=limits" {
		t.Fatalf("save: %d %s", r.StatusCode, r.body)
	}
	if v := e.app.Store.Data().ConfigOverrides["limits"].Value; v != "max_inflight: 77\n" || e.app.Runtime().Config.Limits.MaxInflight != 77 {
		t.Fatalf("stored override: %q", v)
	}
	r := e.get("/admin/config?ok=config.saved&v=limits", s.cookie)
	if !strings.Contains(r.body, "max_inflight: 77") || !strings.Contains(r.body, `action="/admin/config/reset"`) {
		t.Fatal("the configuration page should list the override and allow resetting it")
	}
	checkPage(t, "config", r.body)
	// The limits are also saved on their own page, which they go back to
	form.Set("max_inflight", "78")
	if r := e.post(s, "/admin/limits/save", form); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/limits?ok=config.saved&v=limits" ||
		e.app.Runtime().Config.Limits.MaxInflight != 78 {
		t.Fatalf("save on the limits page: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// Invalid input: redisplayed with the explanation and the filled-in values, on the page that edits them, not saved
	form.Set("max_inflight", "lots")
	for _, path := range []string{"/admin/config/save", "/admin/limits/save"} {
		if r := e.post(s, path, form); r.StatusCode != 400 || !strings.Contains(r.body, `value="lots"`) || !strings.Contains(r.body, `action="/admin/limits/save"`) {
			t.Fatalf("invalid input at %s: %d", path, r.StatusCode)
		}
	}
	form.Set("max_inflight", "77")
	if r := e.post(s, "/admin/limits/save", form); r.StatusCode != 303 {
		t.Fatalf("save: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/config/save", url.Values{"key": {"listen"}}); r.StatusCode != 400 {
		t.Fatalf("a section that cannot be changed: %d", r.StatusCode)
	}
	// Switches: unticked counts as false
	if r := e.post(s, "/admin/site/status", url.Values{"key": {"site"}}); r.StatusCode != 303 || e.app.Runtime().Site.ShowStatus ||
		r.Header.Get("Location") != "/admin/site?ok=config.saved&v=site" {
		t.Fatalf("turn off the service status: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// A reset goes back to the page it was made from
	if r := e.post(s, "/admin/config/reset", url.Values{"key": {"site"}, "back": {"/admin/site"}}); r.StatusCode != 303 || !e.app.Runtime().Site.ShowStatus ||
		r.Header.Get("Location") != "/admin/site?ok=config.reset&v=site" {
		t.Fatalf("reset from the usage page's page: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	// Create an upstream, then add an entrance that always forwards to it
	r = e.post(s, "/admin/upstreams/save", url.Values{"new": {"1"}, "upstream": {"CR.example.com"}, "registry": {"registry.example.com"},
		"token_realm_hosts": {"auth.example.com"}, "unlisted_redirect": {"pass"}})
	if r.StatusCode != 303 || e.app.Runtime().Config.Upstreams["cr.example.com"] == nil {
		t.Fatalf("create upstream: %d %s", r.StatusCode, r.body)
	}
	if r := e.post(s, "/admin/upstreams/save", url.Values{"new": {"1"}, "upstream": {"ghcr.io"}, "registry": {"x.example.com"}}); r.StatusCode != 409 {
		t.Fatalf("same name as an existing one: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/upstreams/save", url.Values{"new": {"1"}, "upstream": {"nodot"}}); r.StatusCode != 400 {
		t.Fatalf("invalid name: %d", r.StatusCode)
	}
	// No token_realm_hosts: saved, but the warning is shown directly
	r = e.post(s, "/admin/upstreams/save", url.Values{"new": {"1"}, "upstream": {"bare.example.com"}, "registry": {"bare.example.com"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, "token_realm_hosts") || !strings.Contains(r.body, "ol-toast-success") {
		t.Fatalf("shown directly when there are warnings: %d", r.StatusCode)
	}
	checkPage(t, "upstream warnings", r.body)
	r = e.get("/admin/entrances/view?new=1", s.cookie)
	checkPage(t, "new entrance", r.body)
	if r := e.post(s, "/admin/entrances/save", url.Values{"new": {"1"}, "hosts": {"cr.test"}, "upstream": {"cr.example.com"},
		"auth": {"none"}, "page": {"off"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/entrances/view?i=1&ok=config.saved&v=entrances" {
		t.Fatalf("add an entrance: %d %s %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	if en := e.app.Runtime().Router.Entrance("cr.test"); en == nil || en.Upstream != "cr.example.com" || en.Page != "off" {
		t.Fatalf("the new entrance should take effect: %+v", en)
	}
	// An upstream referenced by an entrance cannot be deleted
	if r := e.post(s, "/admin/config/reset", url.Values{"key": {"upstreams/cr.example.com"}}); r.StatusCode != 422 {
		t.Fatalf("a referenced upstream cannot be deleted: %d", r.StatusCode)
	}
	// Each entrance has its page; a form made from an entrance that changed since is refused
	r = e.get("/admin/entrances/view?i=1", s.cookie)
	checkPage(t, "entrance", r.body)
	orig := regexp.MustCompile(`name="orig" value="([0-9a-f]+)"`).FindStringSubmatch(r.body)
	if orig == nil || !strings.Contains(r.body, "certs.d/cr.example.com") {
		t.Fatal("the entrance page should carry the form and the client configuration for its upstream")
	}
	if r := e.post(s, "/admin/entrances/delete", url.Values{"i": {"1"}, "orig": {"0000"}}); r.StatusCode != 409 {
		t.Fatalf("a stale form: %d", r.StatusCode)
	}
	if r := e.post(s, "/admin/entrances/delete", url.Values{"i": {"1"}, "orig": {orig[1]}}); r.StatusCode != 303 || e.app.Runtime().Router.Entrance("cr.test").Upstream != "" {
		t.Fatalf("delete entrance: %d", r.StatusCode)
	}
	if _, ok := e.app.Store.Data().ConfigOverrides["entrances"]; ok {
		t.Fatal("an entrance list identical to the configuration file is not stored")
	}
	if r := e.post(s, "/admin/config/reset", url.Values{"key": {"upstreams/cr.example.com"}}); r.StatusCode != 303 || e.app.Runtime().Config.Upstreams["cr.example.com"] != nil {
		t.Fatalf("delete the upstream created in the console: %d", r.StatusCode)
	}

	// Export
	r = e.get("/admin/config/export", s.cookie)
	if r.StatusCode != 200 || !strings.Contains(r.body, "max_inflight: 77") || !strings.Contains(r.Header.Get("Content-Disposition"), "config-overrides.yaml") {
		t.Fatalf("export: %d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/config/export?format=merged", s.cookie); r.StatusCode != 200 || !strings.Contains(r.body, "manifest_rate: 3") {
		t.Fatalf("merged export: %d %s", r.StatusCode, r.body)
	}

	// Read-only role: can see, cannot change
	v := e.login("victor", victorPW)
	r = e.get("/admin/config", v.cookie)
	if strings.Contains(r.body, `action="/admin/config/save"`) || !strings.Contains(r.body, "max_inflight") {
		t.Fatal("the read-only role can only view")
	}
	if r := e.post(v, "/admin/config/save", url.Values{"key": {"limits"}, "max_inflight": {"1"}}); r.StatusCode != 403 {
		t.Fatalf("the read-only role cannot change: %d", r.StatusCode)
	}
	// After 10 minutes re-authentication is required
	e.advance(11 * time.Minute)
	if r := e.post(s, "/admin/config/save", form); r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/admin/reauth") {
		t.Fatalf("configuration changes require a recently entered password: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

// Configuration errors are shown in the interface language, including the part that comes from validating
// the configuration.
func TestLocalizedConfigErrors(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	ent := url.Values{"_csrf": {s.csrf}, "new": {"1"}, "name": {"x"}, "hosts": {"x.test"}, "upstream": {"nope.example.com"},
		"auth": {"none"}, "page": {"public"}}
	for _, lang := range []i18n.Lang{i18n.EN, i18n.ZH} {
		r := e.do("POST", "/admin/entrances/save", ent, []string{s.cookie, langCookie + "=" + string(lang)}, nil)
		if r.StatusCode != 422 || !strings.Contains(r.body, i18n.T(lang, "The configuration was not saved")) {
			t.Fatalf("%s: %d", lang, r.StatusCode)
		}
		if english := strings.Contains(r.body, "is not a configured upstream"); english != (lang == i18n.EN) {
			t.Errorf("%s: the validation error is not in the interface language", lang)
		}
	}
}

func TestHead(t *testing.T) {
	e := newEnv(t, envOpts{})
	if r := e.do("HEAD", "/admin/login", nil, nil, nil); r.StatusCode != 200 || r.body != "" {
		t.Fatalf("HEAD /admin/login: %d %q", r.StatusCode, r.body)
	}
	if r := e.do("HEAD", "/admin/users", nil, nil, nil); r.StatusCode != 303 {
		t.Fatalf("HEAD of a page that needs a session redirects like GET: %d", r.StatusCode)
	}
	if r := e.do("PUT", "/admin/login", nil, nil, nil); r.StatusCode != 405 || r.Header.Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("PUT: %d %s", r.StatusCode, r.Header.Get("Allow"))
	}
}
