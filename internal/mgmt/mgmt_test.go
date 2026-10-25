package mgmt

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// newApp builds an App from a configuration file and returns the App and the file's path (rewrite the
// file, then call ReloadConfig).
func newApp(t *testing.T, yaml string) (*app.App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml = "data_dir: " + filepath.ToSlash(dir) + "\nlog: {access: {output: \"off\"}}\nproxy: {health_interval: 0s}\n" + yaml
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(res, []byte(yaml), app.Options{ConfigPath: path, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, path
}

func request(remote, host, path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.RemoteAddr = remote
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestAllowed(t *testing.T) {
	// Default admin.public: anyone can open the console and API (protected by login); metrics and pprof
	// are still only for direct connections from the local machine and private networks
	open, _ := newApp(t, "metrics: {enabled: true}")
	for _, c := range []struct {
		r    *http.Request
		want bool
	}{
		{request("203.0.113.9:5000", "mirror.example.com", "/admin/login", nil), true},
		{request("10.42.0.1:5000", "mirror.example.com", "/admin/api/v1/status", map[string]string{"X-Forwarded-For": "203.0.113.9"}), true},
		{request("10.42.0.1:5000", "mirror.example.com", "/metrics", map[string]string{"X-Forwarded-For": "203.0.113.9"}), false},
		{request("203.0.113.9:5000", "mirror.example.com", "/debug/pprof/", nil), false},
		{request("10.42.3.7:5000", "10.42.0.9:8080", "/metrics", nil), true},
	} {
		if got, reason := Allowed(open.Runtime(), c.r); got != c.want {
			t.Errorf("public: %s %s: want %v, got %v (%s)", c.r.RemoteAddr, c.r.URL.Path, c.want, got, reason)
		}
	}
	private, _ := newApp(t, "admin: {public: false}")
	if ok, _ := Allowed(private.Runtime(), request("203.0.113.9:5000", "mirror.example.com", "/admin/login", nil)); ok {
		t.Fatal("with admin.public: false the console is allowed by address")
	}

	// The address-based rules (metrics, pprof, and the console when public: false)
	plain, _ := newApp(t, "")
	proxied, _ := newApp(t, `
client_ip: {trusted_proxies: [10.42.0.0/16]}
admin:
  public_url: https://admin.example.com
  proxied_allow: [198.51.100.0/24]
`)
	xff := func(v string) map[string]string { return map[string]string{"X-Forwarded-For": v} }
	cases := []struct {
		name string
		a    *app.App
		r    *http.Request
		want bool
	}{
		{"local direct connection (kubectl port-forward)", plain, request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/", nil), true},
		{"private network direct connection (Prometheus, LAN)", plain, request("10.42.3.7:5000", "10.42.0.9:8080", "/metrics", nil), true},
		{"IPv6 local machine", plain, request("[::1]:5000", "[::1]:8080", "/admin/", nil), true},
		{"public direct connection", plain, request("203.0.113.9:5000", "mirror.example.com", "/admin/", nil), false},
		{"private peer but with X-Forwarded-For (proxied)", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", xff("127.0.0.1")), false},
		{"private peer but with Via", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", map[string]string{"Via": "1.1 squid"}), false},
		{"private peer but with X-Forwarded-Proto", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", map[string]string{"X-Forwarded-Proto": "https"}), false},
		{"no proxied access without proxied_allow", plain, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), false},
		{"proxied: allowed client", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), true},
		{"proxied: client not in the list", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("192.0.2.1")), false},
		{"proxied: a forged private address does not work either", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("127.0.0.1")), false},
		{"proxied: Host is not public_url", proxied, request("10.42.0.1:5000", "mirror.example.com", "/admin/", xff("198.51.100.7")), false},
		{"proxied: peer is not a trusted proxy", proxied, request("192.168.1.5:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), false},
		{"proxied: only a Forwarded header, no client address", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", map[string]string{"Forwarded": "for=198.51.100.7"}), false},
	}
	for _, c := range cases {
		if got, reason := Gated(c.a.Runtime(), c.r); got != c.want {
			t.Errorf("%s: want %v, got %v (%s)", c.name, c.want, got, reason)
		}
	}
	old := request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/", nil)
	old.Proto, old.ProtoMajor, old.ProtoMinor = "HTTP/1.0", 1, 0
	if ok, _ := Gated(plain.Runtime(), old); ok {
		t.Fatal("HTTP/1.0 counts as proxied (nginx's default proxy_pass) and is not allowed")
	}
	closed, _ := newApp(t, "admin: {allow: []}")
	if ok, _ := Gated(closed.Runtime(), request("127.0.0.1:5000", "127.0.0.1:8080", "/metrics", nil)); ok {
		t.Fatal("nobody gets in when admin.allow is empty")
	}
}

// pull simulates the pull entrance: 404 for unknown paths, and it records the access log note.
type pull struct{ note string }

func (p *pull) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.note = accesslog.NoteFrom(r.Context())
	http.NotFound(w, r)
}

func serve(h http.Handler, r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestShared(t *testing.T) {
	a, path := newApp(t, "admin: {public: false}\n")
	h, err := New(a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	next := &pull{}
	s := h.Shared(next)

	if code, body := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/login", nil)); code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("a local direct connection should open the login page: %d", code)
	}
	// Default admin.public: the login page opens from the internet through a proxy too
	pub, _ := newApp(t, "")
	ph, err := New(pub, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := serve(ph.Shared(&pull{}), request("10.42.0.1:5000", "mirror.example.com", "/admin/login", map[string]string{"X-Forwarded-For": "203.0.113.9"})); code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("with admin.public the login page opens through a proxy too: %d", code)
	}
	// When rejected, the request goes to the pull entrance; the 404 is the same as for any unknown path,
	// and the access log states the reason
	code, body := serve(s, request("203.0.113.9:5000", "mirror.example.com", "/admin/login", nil))
	if !strings.Contains(next.note, "admin.allow") {
		t.Fatalf("the access log should state the reason: %q", next.note)
	}
	_, plain404 := serve(next, request("203.0.113.9:5000", "mirror.example.com", "/no-such-path", nil))
	if code != 404 || body != plain404 {
		t.Fatalf("a rejected console request should look like a plain 404: %d %q", code, body)
	}
	// Metrics are off by default: a plain 404 even for a local direct connection
	next.note = ""
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/metrics", nil)); code != 404 || next.note != "" {
		t.Fatalf("metrics are off by default: %d %q", code, next.note)
	}
	// Hot reload turns on metrics and pprof
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, []byte("metrics: {enabled: true}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if code, body := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/metrics", nil)); code != 200 || !strings.Contains(body, "relay_") {
		t.Fatalf("metrics should be scrapable once turned on: %d", code)
	}
	if code, _ := serve(s, request("203.0.113.9:5000", "mirror.example.com", "/metrics", nil)); code != 404 {
		t.Fatalf("metrics are still not scrapable from the internet: %d", code)
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/debug/pprof/", nil)); code != 404 {
		t.Fatalf("404 while pprof is off: %d", code)
	}
	if err := os.WriteFile(path, append(b, []byte("metrics: {enabled: true, pprof: true}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/debug/pprof/", nil)); code != 200 {
		t.Fatalf("should open once pprof is on: %d", code)
	}
	// Other paths go to the pull entrance as usual
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/v2/", nil)); code != 404 || next.note != "" {
		t.Fatal("non-management paths should go to the pull entrance")
	}
}

func TestStandalone(t *testing.T) {
	a, _ := newApp(t, `listen: {admin: "127.0.0.1:9090"}
admin: {public: false}`)
	h, err := New(a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := h.Standalone(slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, request("127.0.0.1:5000", "127.0.0.1:9090", "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("the root path should redirect to the console: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:9090", "/admin/login", nil)); code != 200 {
		t.Fatalf("a local direct connection should open it: %d", code)
	}
	for _, r := range []*http.Request{
		request("203.0.113.9:5000", "127.0.0.1:9090", "/admin/login", nil),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/admin/login", map[string]string{"X-Forwarded-For": "127.0.0.1"}),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/v2/", nil),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/metrics", nil),
	} {
		if code, _ := serve(s, r); code != 404 {
			t.Errorf("%s %s should be 404: %d", r.RemoteAddr, r.URL.Path, code)
		}
	}
}
