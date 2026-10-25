package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

const (
	adminTok  = "admin-token-0123456789abcdef0123456789"
	viewerTok = "viewer-token-0123456789abcdef012345678"
)

type env struct {
	srv *httptest.Server
	app *app.App
}

func newEnv(t *testing.T, st store.Store) *env {
	t.Helper()
	t.Setenv("T_ADMIN", adminTok)
	t.Setenv("T_VIEWER", viewerTok)
	raw := []byte(`
log: {access: {output: "off"}}
admin:
  api_tokens:
    - {name: ops, role: admin, token: {env: T_ADMIN}}
    - {name: grafana, role: viewer, token: {env: T_VIEWER}}
`)
	res, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		bs, err := store.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		st = bs
	}
	a, err := app.New(res, raw, app.Options{Log: slog.New(slog.DiscardHandler), Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e := &env{app: a, srv: httptest.NewServer(New(a, ops.New(a), Options{}))}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) call(t *testing.T, method, path, token string, body any) (int, map[string]any, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+Prefix+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func TestAuthAndRoles(t *testing.T) {
	e := newEnv(t, nil)
	if code, _, _ := e.call(t, "GET", "/status", "", nil); code != 401 {
		t.Fatalf("no token should return 401: %d", code)
	}
	if code, _, _ := e.call(t, "GET", "/status", viewerTok, nil); code != 200 {
		t.Fatalf("viewer can read: %d", code)
	}
	if code, m, _ := e.call(t, "GET", "/status/report", viewerTok, nil); code != 200 || m["status"] == nil || m["upstreams"] == nil {
		t.Fatalf("service status report: %d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/bans", viewerTok, map[string]string{"cidr": "198.51.100.7", "duration": "1h"}); code != 403 {
		t.Fatalf("viewer cannot write: %d", code)
	}
	// With admin.public (the default) any domain works; for the 421 with public: false see TestHostCheck in
	// the admin package
	req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/status", nil)
	req.Host = "mirror.example.test"
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("API reached by domain name with admin.public: %d", resp.StatusCode)
	}
	// Too many failed authentications
	var last int
	for i := 0; i < 12; i++ {
		last, _, _ = e.call(t, "GET", "/status", "wrong-token", nil)
	}
	if last != 429 {
		t.Fatalf("too many failed authentications should return 429: %d", last)
	}
}

func TestUsers(t *testing.T) {
	e := newEnv(t, nil)
	code, _, raw := e.call(t, "POST", "/users", adminTok, map[string]any{"name": "ci", "password": "correct horse battery", "manifest_rate": 10})
	if code != 201 {
		t.Fatalf("create user: %d %s", code, raw)
	}
	u := e.app.Runtime().Users["ci"]
	if u == nil || u.Source != "store" || u.ManifestRPS != 10 {
		t.Fatalf("the user should take effect immediately: %+v", u)
	}
	if ok, _ := e.app.Auth.Verify(u, "correct horse battery"); !ok {
		t.Fatal("the password should verify")
	}
	if code, _, _ := e.call(t, "POST", "/users", adminTok, map[string]any{"name": "ci", "password": "correct horse battery"}); code != 409 {
		t.Fatal("a duplicate name should return 409")
	}
	for _, b := range []map[string]any{
		{"name": "bad name", "password": "correct horse battery"},
		{"name": "x", "password": "short"},
		{"name": "x"},
		{"name": "x", "hash": "{SHA}abc"},
		{"name": "x", "password": "correct horse battery", "extra": 1},
	} {
		if code, _, _ := e.call(t, "POST", "/users", adminTok, b); code != 400 {
			t.Errorf("should reject %v: %d", b, code)
		}
	}
	if code, _, _ := e.call(t, "PATCH", "/users/ci", adminTok, map[string]any{"disabled": true}); code != 200 || !e.app.Runtime().Users["ci"].Disabled {
		t.Fatal("disabling should take effect")
	}
	code, _, raw = e.call(t, "GET", "/users", viewerTok, nil)
	if code != 200 || strings.Contains(string(raw), "$2a$") || !strings.Contains(string(raw), `"ci"`) {
		t.Fatalf("the list should not contain hashes: %s", raw)
	}
	if code, _, _ := e.call(t, "DELETE", "/users/ci", adminTok, nil); code != 204 || e.app.Runtime().Users["ci"] != nil {
		t.Fatal("the delete should take effect")
	}
	if code, _, _ := e.call(t, "DELETE", "/users/ci", adminTok, nil); code != 404 {
		t.Fatal("deleting a user that does not exist should return 404")
	}
}

func TestRulesAndBans(t *testing.T) {
	e := newEnv(t, nil)
	ip := netip.MustParseAddr("198.51.100.7")
	if code, _, raw := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "198.51.100.7", "duration": "2h", "reason": "traffic abuse"}); code != 201 {
		t.Fatalf("ban: %d %s", code, raw)
	}
	if ok, _ := e.app.Runtime().IPRules.Decide(ip, time.Now()); ok {
		t.Fatal("the ban should take effect immediately")
	}
	if code, _, _ := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "0.0.0.0/0", "duration": "1h"}); code != 400 {
		t.Fatal("must not ban every address")
	}
	if code, _, _ := e.call(t, "DELETE", "/bans?cidr=198.51.100.7", adminTok, nil); code != 204 {
		t.Fatal("unban")
	}
	if ok, _ := e.app.Runtime().IPRules.Decide(ip, time.Now()); !ok {
		t.Fatal("the unban should take effect immediately")
	}

	if code, _, raw := e.call(t, "POST", "/rules/image", adminTok, map[string]any{"pattern": "ghcr.io/evil/**", "kind": "deny", "status": 451, "message": "blocked after a complaint"}); code != 201 {
		t.Fatalf("add image rule: %d %s", code, raw)
	}
	_, m, _ := e.call(t, "POST", "/rules/image/test", viewerTok, map[string]string{"image": "ghcr.io/evil/miner:latest"})
	if m["allowed"] != false || m["status"].(float64) != 451 || m["image"] != "ghcr.io/evil/miner" {
		t.Fatalf("test image: %v", m)
	}
	_, m, _ = e.call(t, "POST", "/rules/image/test", viewerTok, map[string]string{"image": "nginx:1.27"})
	if m["allowed"] != true || m["image"] != "docker.io/library/nginx" {
		t.Fatalf("Docker Hub when no upstream is given: %v", m)
	}
	if code, _, _ := e.call(t, "POST", "/rules/image", adminTok, map[string]any{"pattern": "example.org/x", "kind": "deny"}); code != 400 {
		t.Fatal("an upstream that is not configured should be rejected")
	}
	if code, _, _ := e.call(t, "POST", "/rules/ip", adminTok, map[string]any{"cidr": "203.0.113.0/24", "kind": "deny"}); code != 201 {
		t.Fatal("add IP rule")
	}
	_, m, _ = e.call(t, "GET", "/rules", viewerTok, nil)
	if len(m["images"].([]any)) != 1 || len(m["ip"].([]any)) != 1 {
		t.Fatalf("rule list: %v", m)
	}
	// Audit log
	code, _, raw := e.call(t, "GET", "/audit", viewerTok, nil)
	if code != 200 || !strings.Contains(string(raw), `"ban.add"`) || !strings.Contains(string(raw), `"token:ops"`) {
		t.Fatalf("audit log: %s", raw)
	}
}

func TestTokensAndRedirectHosts(t *testing.T) {
	e := newEnv(t, nil)
	_, m, _ := e.call(t, "POST", "/tokens", adminTok, map[string]string{"name": "ci", "role": "viewer"})
	tok, _ := m["token"].(string)
	if !strings.HasPrefix(tok, "rmr_") {
		t.Fatalf("should return the new token: %v", m)
	}
	if code, _, _ := e.call(t, "GET", "/status", tok, nil); code != 200 {
		t.Fatal("the new token should work")
	}
	if code, _, _ := e.call(t, "POST", "/tokens", adminTok, map[string]string{"name": "ops", "role": "viewer"}); code != 409 {
		t.Fatal("a name already used by a configured token should be rejected")
	}
	if code, _, _ := e.call(t, "DELETE", "/tokens/ci", adminTok, nil); code != 204 {
		t.Fatal("delete token")
	}
	if code, _, _ := e.call(t, "GET", "/status", tok, nil); code != 401 {
		t.Fatal("a deleted token must no longer work")
	}

	if code, _, raw := e.call(t, "POST", "/upstreams/quay.io/redirect-hosts", adminTok, map[string]string{"host": "cdn99.quay.io"}); code != 201 {
		t.Fatalf("add redirect host: %d %s", code, raw)
	}
	if code, _, _ := e.call(t, "POST", "/upstreams/quay.io/redirect-hosts", adminTok, map[string]string{"host": "*.evil.com"}); code != 400 {
		t.Fatal("wildcards are not accepted")
	}
	_, _, raw := e.call(t, "GET", "/upstreams", viewerTok, nil)
	if !strings.Contains(string(raw), `cdn99\\.quay\\.io`) {
		t.Fatalf("the allowlist should include the new host (escaped as a regular expression): %s", raw)
	}
}

func TestReadOnlyStoreAndMisc(t *testing.T) {
	e := newEnv(t, store.NewReadOnly())
	if code, m, _ := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "198.51.100.7", "duration": "1h"}); code != 409 || m["code"] != "store.readonly" || !strings.Contains(m["error"].(string), "read-only") {
		t.Fatalf("a read-only store should return 409: %d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/reload", adminTok, map[string]string{}); code != 422 {
		t.Fatalf("reload without a configuration file should return 422: %d", code)
	}
	for _, p := range []string{"/stats/series?minutes=10", "/stats/summary?hours=24", "/requests?limit=5", "/config", "/tokens", "/upstreams"} {
		if code, _, raw := e.call(t, "GET", p, viewerTok, nil); code != 200 {
			t.Errorf("%s: %d %s", p, code, raw)
		}
	}
	_, m, _ := e.call(t, "GET", "/config", viewerTok, nil)
	if y := m["yaml"].(string); strings.Contains(y, adminTok) || !strings.Contains(y, "T_ADMIN") {
		t.Fatal("the configuration must not contain plain-text tokens")
	}
	// A request body that is not JSON
	req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/bans", strings.NewReader("cidr=1"))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON should return 415: %d", resp.StatusCode)
	}
}

func TestAdminsAndSite(t *testing.T) {
	e := newEnv(t, nil)
	if code, _, raw := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "alice", "password": "alice-password-1", "role": "admin"}); code != 201 {
		t.Fatalf("create administrator: %d %s", code, raw)
	}
	if code, _, _ := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "alice", "password": "alice-password-1", "role": "admin"}); code != 409 {
		t.Fatal("a duplicate name should return 409")
	}
	if code, _, _ := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "bob", "password": "short", "role": "admin"}); code != 400 {
		t.Fatal("a password that is too short should return 400")
	}
	if code, m, _ := e.call(t, "PATCH", "/admins/alice", adminTok, map[string]any{"role": "viewer"}); code != 409 || m["code"] != "admin.last" || !strings.Contains(m["error"].(string), "At least one") {
		t.Fatalf("cannot remove the last admin: %d %v", code, m)
	}
	_, _, raw := e.call(t, "GET", "/admins", viewerTok, nil)
	if strings.Contains(string(raw), "$2a$") || !strings.Contains(string(raw), `"alice"`) {
		t.Fatalf("the list contains no hashes: %s", raw)
	}
	if code, _, _ := e.call(t, "DELETE", "/admins/alice", adminTok, nil); code != 409 {
		t.Fatal("cannot delete the last admin")
	}

	code, m, raw := e.call(t, "PUT", "/site", adminTok, map[string]any{"name": "Test Mirrors", "operator": "Test Org", "links": map[string]string{"status": "https://status.example.org"}})
	if code != 200 || m["source"] != "store" || e.app.Runtime().Site.Name != "Test Mirrors" {
		t.Fatalf("site information: %d %s", code, raw)
	}
	if code, _, _ := e.call(t, "PUT", "/site", adminTok, map[string]any{"name": "x", "links": map[string]string{"status": "javascript:alert(1)"}}); code != 400 {
		t.Fatal("links accept only http(s)")
	}
	if code, m, _ := e.call(t, "DELETE", "/site", adminTok, nil); code != 200 || m["source"] != "config" {
		t.Fatalf("back to the configuration file: %d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/upstreams/nope.example/check", adminTok, nil); code != 404 {
		t.Fatalf("probe an upstream that does not exist: %d", code)
	}
}

func TestConfigOverrides(t *testing.T) {
	e := newEnv(t, nil)
	code, m, raw := e.call(t, "PUT", "/config/overrides/limits", adminTok, map[string]any{"max_inflight": 50, "manifest_rate": 2})
	if code != 200 || e.app.Runtime().Config.Limits.MaxInflight != 50 {
		t.Fatalf("change limits: %d %s", code, raw)
	}
	// manifest_rate is the same as in the configuration file (the default), so it is not stored
	if ov := m["override"].(map[string]any); ov["state"] != "active" || strings.Contains(ov["value"].(string), "manifest_rate") {
		t.Fatalf("returns the stored override: %v", ov)
	}
	code, _, raw = e.call(t, "PUT", "/config/overrides/upstreams/cr.example.com", adminTok, map[string]any{"registry": "registry.example.com"})
	if code != 200 || !strings.Contains(string(raw), "token_realm_hosts") {
		t.Fatalf("create upstream, warnings returned: %d %s", code, raw)
	}
	if code, _, _ := e.call(t, "PUT", "/config/overrides/entrances", adminTok, []map[string]any{{"hosts": []string{"cr.example.com"}, "upstream": "cr.example.com"}, {"hosts": []string{"*"}}}); code != 200 {
		t.Fatalf("change entrances: %d", code)
	}
	if e := e.app.Runtime().Router.Entrance("cr.example.com"); e == nil || e.Upstream != "cr.example.com" {
		t.Fatal("the new entrance should take effect immediately")
	}
	for _, c := range []struct {
		path string
		body any
		code int
	}{
		{"/config/overrides/listen", map[string]any{"http": ":80"}, 422},
		{"/config/overrides/admin", map[string]any{"allow": []string{"0.0.0.0/0"}}, 422},
		{"/config/overrides/limits", map[string]any{"tracked_clients": 1}, 422},
		{"/config/overrides/entrances", []map[string]any{{"hosts": []string{"x.example.com"}, "upstream": "nope.example.com"}}, 422},
	} {
		if code, m, _ := e.call(t, "PUT", c.path, adminTok, c.body); code != c.code || m["code"] != "config.invalid" || !strings.Contains(m["error"].(string), "not saved") {
			t.Errorf("%s: %d %v", c.path, code, m)
		}
	}
	if code, _, _ := e.call(t, "PUT", "/config/overrides/limits", viewerTok, map[string]any{"max_inflight": 1}); code != 403 {
		t.Fatal("viewer cannot change")
	}
	code, m, _ = e.call(t, "GET", "/config/overrides", viewerTok, nil)
	if code != 200 || m["editable"] != true || len(m["overrides"].([]any)) != 3 {
		t.Fatalf("list: %d %v", code, m)
	}
	_, m, _ = e.call(t, "GET", "/config", viewerTok, nil)
	if !strings.Contains(m["yaml"].(string), "max_inflight: 50") {
		t.Fatal("the effective configuration includes the console overrides")
	}
	_, m, _ = e.call(t, "GET", "/config?view=file", viewerTok, nil)
	if strings.Contains(m["yaml"].(string), "max_inflight: 50") {
		t.Fatal("view=file uses only the configuration file")
	}
	_, m, _ = e.call(t, "GET", "/config/export", viewerTok, nil)
	if y := m["yaml"].(string); !strings.Contains(y, "max_inflight: 50") || !strings.Contains(y, "cr.example.com") {
		t.Fatalf("export: %s", y)
	}
	_, m, _ = e.call(t, "GET", "/config/export?format=merged", viewerTok, nil)
	if y := m["yaml"].(string); !strings.Contains(y, "T_ADMIN") || !strings.Contains(y, "max_inflight: 50") {
		t.Fatalf("merged export: %s", y)
	}
	if code, _, _ := e.call(t, "GET", "/config/export?format=zip", viewerTok, nil); code != 400 {
		t.Fatal("unknown format")
	}
	if code, _, _ := e.call(t, "DELETE", "/config/overrides/limits", adminTok, nil); code != 204 || e.app.Runtime().Config.Limits.MaxInflight == 50 {
		t.Fatal("after a reset the configuration file's value applies again")
	}
	if code, _, _ := e.call(t, "DELETE", "/config/overrides/limits", adminTok, nil); code != 404 {
		t.Fatal("resetting something unchanged returns 404")
	}
	_, _, raw = e.call(t, "GET", "/audit", viewerTok, nil)
	if !strings.Contains(string(raw), `"config.override.set"`) || !strings.Contains(string(raw), `"config.override.reset"`) {
		t.Fatalf("audit log: %s", raw)
	}
	// Read-only store
	ro := newEnv(t, store.NewReadOnly())
	if code, _, _ := ro.call(t, "PUT", "/config/overrides/limits", adminTok, map[string]any{"max_inflight": 5}); code != 409 {
		t.Fatalf("read-only store: %d", code)
	}
}

func TestErrorLanguage(t *testing.T) {
	e := newEnv(t, store.NewReadOnly())
	get := func(accept string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/status", nil)
		if accept != "" {
			req.Header.Set("Accept-Language", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	for accept, lang := range map[string]i18n.Lang{"": i18n.EN, "fr": i18n.EN, "zh-CN,zh;q=0.9": i18n.ZH} {
		code, m := get(accept)
		if code != 401 || m["code"] != "auth.required" || m["error"] != i18n.T(lang, "A valid API token is required.") {
			t.Errorf("Accept-Language %q: %d %v", accept, code, m)
		}
	}

	req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/bans", strings.NewReader(`{"cidr": "198.51.100.7", "duration": "1h"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("Accept-Language", "zh")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if want := ops.NewError(409, "store.readonly").In(i18n.ZH); resp.StatusCode != 409 || m["code"] != "store.readonly" || m["error"] != want {
		t.Errorf("ops errors in Chinese: %d %v, want %q", resp.StatusCode, m, want)
	}
}
