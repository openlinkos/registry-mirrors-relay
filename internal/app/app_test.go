package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/status"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

func newApp(t *testing.T, yaml string) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml = "data_dir: " + filepath.ToSlash(dir) + "\nlog: {access: {output: \"off\"}}\n" + yaml
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(res, []byte(yaml), Options{ConfigPath: path, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, path
}

func TestReloadKeepsPoolWhenUpstreamsUnchanged(t *testing.T) {
	a, path := newApp(t, "limits:\n  manifest_rate: 2\n")
	first := a.Runtime()
	if err := os.WriteFile(path, []byte("data_dir: "+filepath.ToSlash(filepath.Dir(path))+"\nlog: {access: {output: \"off\"}}\nlimits:\n  manifest_rate: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	second := a.Runtime()
	if second == first || second.Config.Limits.ManifestRate != 5 {
		t.Fatal("reload should switch to a new Runtime")
	}
	if second.Pool != first.Pool || second.Manifests != first.Manifests {
		t.Fatal("upstream and cache settings are unchanged; the old connection pool and cache should be reused")
	}
	if len(a.LastReload().Restart) != 0 {
		t.Fatalf("no restart should be needed: %v", a.LastReload().Restart)
	}

	// Changing upstreams: a new connection pool; changing listen: a restart notice
	cfg2 := "data_dir: " + filepath.ToSlash(filepath.Dir(path)) + "\nlog: {access: {output: \"off\"}}\nlisten: {http: \":9999\"}\nupstreams:\n  quay.io: {unlisted_redirect: deny}\n"
	_ = os.WriteFile(path, []byte(cfg2), 0o600)
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	third := a.Runtime()
	if third.Pool == second.Pool {
		t.Fatal("changed upstream settings should create a new connection pool")
	}
	if r := a.LastReload().Restart; len(r) != 1 || r[0] != "listen" {
		t.Fatalf("changing the listen address should call for a restart: %v", r)
	}

	// Bad configuration: keep the previous settings
	_ = os.WriteFile(path, []byte("listen: {htttp: x}\n"), 0o600)
	if err := a.ReloadConfig(); err == nil || a.Runtime() != third || a.LastReload().Err == "" {
		t.Fatal("a bad configuration should not replace the settings in effect")
	}
}

func TestUsersFromFileAndStore(t *testing.T) {
	dir := t.TempDir()
	h, _ := bcrypt.GenerateFromPassword([]byte("pw-from-file"), bcrypt.MinCost)
	users := filepath.Join(dir, "htpasswd")
	_ = os.WriteFile(users, []byte("alice:"+string(h)+"\n"), 0o600)
	a, _ := newApp(t, "access:\n  users_file: "+filepath.ToSlash(users)+"\n")
	if u := a.Runtime().Users["alice"]; u == nil || u.Source != "config" {
		t.Fatalf("users_file user: %+v", u)
	}
	err := a.Store.Update(func(d *store.Data) error {
		d.Users = append(d.Users, store.ClientUser{Name: "alice", Hash: "$2a$x"}, store.ClientUser{Name: "bob", Hash: string(h)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Rebuild(); err != nil {
		t.Fatal(err)
	}
	rt := a.Runtime()
	if rt.Users["alice"].Source != "config" || rt.Users["bob"] == nil || rt.Users["bob"].Source != "store" {
		t.Fatal("on a name clash the configuration file should win, the rest come from the operational database")
	}
}

func TestBootstrapAdminAndTokens(t *testing.T) {
	t.Setenv("T_PW", "initial-admin-password")
	t.Setenv("T_TOK", strings.Repeat("t", 40))
	a, _ := newApp(t, "admin:\n  initial_admin: {username: root, password: {env: T_PW}}\n  api_tokens: [{name: ops, role: admin, token: {env: T_TOK}}]\n")
	admins := a.Store.Data().Admins
	if len(admins) != 1 || admins[0].Name != "root" || bcrypt.CompareHashAndPassword([]byte(admins[0].Hash), []byte("initial-admin-password")) != nil {
		t.Fatalf("the first administrator should be created: %+v", admins)
	}
	if tk, ok := a.Runtime().Tokens[sha256.Sum256([]byte(strings.Repeat("t", 40)))]; !ok || tk.Role != "admin" || tk.Source != "config" {
		t.Fatalf("token from the configuration: %+v", tk)
	}
	sum := sha256.Sum256([]byte("stored-token"))
	_ = a.Store.Update(func(d *store.Data) error {
		d.APITokens = append(d.APITokens, store.APIToken{Name: "ci", Hash: hex.EncodeToString(sum[:]), Role: "viewer"})
		return nil
	})
	_ = a.Rebuild()
	if tk := a.Runtime().Tokens[sum]; tk.Name != "ci" || tk.Source != "store" {
		t.Fatalf("token from the operational database: %+v", tk)
	}
}

func TestBadStoreRulesIgnored(t *testing.T) {
	a, _ := newApp(t, "")
	_ = a.Store.Update(func(d *store.Data) error {
		d.IPRules = append(d.IPRules, store.IPRule{CIDR: "not-an-ip", Kind: "deny"})
		return nil
	})
	if err := a.Rebuild(); err != nil {
		t.Fatalf("a malformed rule should be ignored rather than fail the whole service: %v", err)
	}
}

func TestStatusReport(t *testing.T) {
	a, _ := newApp(t, "")
	r := a.StatusReport()
	if r.Status != "unknown" || len(r.Upstreams) != 5 || r.Interval != 60 {
		t.Fatalf("before any probe the status should be unknown, listing the 5 built-in upstreams: %+v", r)
	}
	for _, u := range r.Upstreams {
		if u.Status != "unknown" || len(u.Hours) != status.Hours {
			t.Fatalf("%s: %+v", u.Name, u)
		}
	}
	if len(r.Service.Hours) != status.Hours {
		t.Fatalf("the service should have %d hours, got %d", status.Hours, len(r.Service.Hours))
	}
}

func TestRequestHours(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	cur := now.Truncate(time.Hour).Unix()
	hs := requestHours([]stats.Minute{
		{Start: cur + 60, Requests: 10, Status5xx: 1},
		{Start: cur + 120, Requests: 5},
		{Start: cur - 3600 + 60, Requests: 7, Status5xx: 7},
		{Start: cur - 30*3600, Requests: 99}, // beyond 24 hours
	}, now)
	if len(hs) != status.Hours || hs[status.Hours-1].Start != cur || hs[status.Hours-1].Requests != 15 || hs[status.Hours-1].Status5xx != 1 {
		t.Fatalf("current hour: %+v", hs[status.Hours-1])
	}
	if h := hs[status.Hours-2]; h.Requests != 7 || h.Status5xx != 7 {
		t.Fatalf("previous hour: %+v", h)
	}
	var total uint64
	for _, h := range hs {
		total += h.Requests
	}
	if total != 22 {
		t.Fatalf("counts older than 24 hours should not be included: %d", total)
	}
}

func TestRestartNeededFields(t *testing.T) {
	base := func() *config.Config {
		res, err := config.Parse(nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.Config
	}
	for name, change := range map[string]func(c *config.Config){
		"log.level":               func(c *config.Config) { c.Log.Level = "debug" },
		"timeouts.read_header":    func(c *config.Config) { c.Timeouts.ReadHeader = config.Duration(time.Minute) },
		"timeouts.idle":           func(c *config.Config) { c.Timeouts.Idle = config.Duration(time.Hour) },
		"timeouts.shutdown_grace": func(c *config.Config) { c.Timeouts.ShutdownGrace = config.Duration(time.Hour) },
		"proxy.buffer_size":       func(c *config.Config) { c.Proxy.BufferSize = 64 << 10 },
		"listen":                  func(c *config.Config) { c.Listen.Admin = "127.0.0.1:9090" },
	} {
		old, cur := base(), base()
		change(cur)
		if got := restartNeeded(old, cur); !slices.Contains(got, name) {
			t.Errorf("changing %s should call for a restart: %v", name, got)
		}
	}
	old, cur := base(), base()
	cur.Metrics.Enabled, cur.Metrics.Pprof = true, true
	cur.Admin.Allow = []string{"loopback"}
	if got := restartNeeded(old, cur); len(got) != 0 {
		t.Fatalf("the metrics switches and admin.allow can be hot reloaded: %v", got)
	}
}

// TestGenerateAdmin: serve creates the first administrator with a generated password, logged once.
func TestGenerateAdmin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "data_dir: " + filepath.ToSlash(dir) + "\nlog: {access: {output: \"off\"}}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	start := func(generate bool) (*App, string) {
		t.Helper()
		res, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		var logs strings.Builder
		a, err := New(res, []byte(yaml), Options{ConfigPath: path, GenerateAdmin: generate, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return a, logs.String()
	}

	a, logs := start(false)
	if n := len(a.Store.Data().Admins); n != 0 || strings.Contains(logs, `"password"`) {
		t.Fatalf("without GenerateAdmin nothing is created: %d admins", n)
	}
	a.Close()

	a, logs = start(true)
	admins := a.Store.Data().Admins
	if len(admins) != 1 || admins[0].Name != "admin" || !admins[0].MustChange || admins[0].Role != "admin" {
		t.Fatalf("generated administrator: %+v", admins)
	}
	var line struct {
		Username, Password string
	}
	for l := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(l, `"password"`) {
			_ = json.Unmarshal([]byte(l), &line)
		}
	}
	if line.Username != "admin" || bcrypt.CompareHashAndPassword([]byte(admins[0].Hash), []byte(line.Password)) != nil {
		t.Fatalf("the logged password should sign in: %q", logs)
	}
	a.Close()

	a, logs = start(true)
	defer a.Close()
	if len(a.Store.Data().Admins) != 1 || strings.Contains(logs, `"password"`) {
		t.Fatal("a restart must not create or log another password")
	}
}

func TestGenerateAdminSkipped(t *testing.T) {
	t.Setenv("RELAY_ADMIN_PW", "initial-admin-password")
	for name, yaml := range map[string]string{
		"initial_admin": "admin: {initial_admin: {username: root, password: {env: RELAY_ADMIN_PW}}}\n",
		"read-only":     "admin: {store: {driver: none}}\n",
	} {
		dir := t.TempDir()
		res, err := config.Parse([]byte("data_dir: " + filepath.ToSlash(dir) + "\nlog: {access: {output: \"off\"}}\n" + yaml))
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(res, nil, Options{GenerateAdmin: true, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		for _, ad := range a.Store.Data().Admins {
			if ad.Name == "admin" {
				t.Errorf("%s: an administrator was generated", name)
			}
		}
		a.Close()
	}
}
