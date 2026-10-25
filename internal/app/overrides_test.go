package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

func ptr(s string) *string { return &s }

// rewrite changes the configuration file and reloads it.
func rewrite(t *testing.T, a *App, path, yaml string) {
	t.Helper()
	yaml = "data_dir: " + filepath.ToSlash(filepath.Dir(path)) + "\nlog: {access: {output: \"off\"}}\n" + yaml
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
}

func stateOf(a *App, key string) OverrideState {
	for _, s := range a.Runtime().Overrides {
		if s.Key == key {
			return s
		}
	}
	return OverrideState{}
}

func dataJSON(t *testing.T, a *App) string {
	t.Helper()
	b, err := json.Marshal(a.Store.Data())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetOverride(t *testing.T) {
	a, _ := newApp(t, "limits:\n  manifest_rate: 2\n")
	first := a.Runtime()
	if _, err := a.SetOverride("limits", ptr("manifest_rate: 7\nmanifest_burst: 20"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	rt := a.Runtime()
	if rt.Config.Limits.ManifestRate != 7 || rt.Config.Limits.ManifestBurst != 20 || rt.FileConfig.Limits.ManifestRate != 2 {
		t.Fatalf("the layered configuration should be in effect and FileConfig the file's: %+v %+v", rt.Config.Limits, rt.FileConfig.Limits)
	}
	if rt.Pool != first.Pool {
		t.Fatal("only the limits changed; the connection pool should be reused")
	}
	saved := a.Store.Data().ConfigOverrides["limits"]
	if saved.UpdatedBy != "admin:alice" || saved.Base == "" || !strings.Contains(saved.Value, "manifest_burst") {
		t.Fatalf("stored override: %+v", saved)
	}
	if st := stateOf(a, "limits"); st.State != OverrideActive {
		t.Fatalf("state: %+v", st)
	}

	// Fields equal to the configuration file are not stored; when all are equal it amounts to removal
	if _, err := a.SetOverride("limits", ptr("manifest_rate: 2\nmanifest_burst: 20"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if v := a.Store.Data().ConfigOverrides["limits"].Value; strings.Contains(v, "manifest_rate") {
		t.Fatalf("fields equal to the configuration file should not be stored: %q", v)
	}
	if _, err := a.SetOverride("limits", ptr("manifest_rate: 2"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Store.Data().ConfigOverrides["limits"]; ok || len(a.Runtime().Overrides) != 0 {
		t.Fatal("the override should be removed when everything equals the configuration file")
	}

	// Changing the upstream connection settings: a new connection pool
	before := a.Runtime()
	if _, err := a.SetOverride("proxy", ptr("max_redirects: 5"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if a.Runtime().Pool == before.Pool {
		t.Fatal("changing proxy should create a new connection pool")
	}
	// Remove
	if _, err := a.SetOverride("proxy", nil, "admin:alice"); err != nil || a.Runtime().Config.Proxy.MaxRedirects != 3 {
		t.Fatalf("after a reset the configuration file's value should apply: %v %d", err, a.Runtime().Config.Proxy.MaxRedirects)
	}

	// Metrics
	_, _ = a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice")
	rec := httptest.NewRecorder()
	a.Metrics.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `relay_config_overrides{key="cache",state="active"} 1`) {
		t.Fatalf("metrics:\n%s", rec.Body.String())
	}
}

func TestSetOverrideRejectedChangesNothing(t *testing.T) {
	a, _ := newApp(t, "")
	_, _ = a.SetOverride("limits", ptr("max_inflight: 50"), "admin:alice")
	data, rt := dataJSON(t, a), a.Runtime()
	var oe *OverrideError
	for _, c := range []struct{ key, value string }{
		{"entrances", "- {hosts: [a.example.com], upstream: nope.example.com}"}, // fails validation when combined
		{"limits", "tracked_clients: 5"},                                        // a field that cannot be changed
		{"listen", "http: :80"},                                                 // a section that cannot be changed
		{"cache", "tag_ttl: soon"},                                              // malformed
	} {
		if _, err := a.SetOverride(c.key, ptr(c.value), "admin:alice"); !errors.As(err, &oe) {
			t.Fatalf("%s should be rejected: %v", c.key, err)
		}
		if dataJSON(t, a) != data || a.Runtime() != rt {
			t.Fatalf("%s: a rejected override must change neither the operational database nor the effective settings", c.key)
		}
	}
	// Removing an override that does not exist: nothing happens
	if _, err := a.SetOverride("cache", nil, "admin:alice"); err != nil || a.Runtime() != rt {
		t.Fatalf("removing an override that does not exist: %v", err)
	}
	// New warnings
	w, err := a.SetOverride("upstreams/cr.example.com", ptr("registry: registry.example.com"), "admin:alice")
	if err != nil || !strings.Contains(fmt.Sprint(w), "token_realm_hosts") {
		t.Fatalf("should return the warnings this change brings: %v %v", w, err)
	}
}

func TestOverridesAgainstFileChanges(t *testing.T) {
	a, path := newApp(t, "limits:\n  manifest_rate: 2\n")
	for _, c := range []struct{ key, value string }{
		{"limits", "manifest_rate: 7"},
		{"upstreams/ghcr.io", "disabled: true"},
		{"upstreams/cr.example.com", "registry: registry.example.com\ntoken_realm_hosts: [auth.example.com]"},
		{"entrances", "- {hosts: [mirror.example.com]}\n- {hosts: [cr.example.com], upstream: cr.example.com}"},
	} {
		if _, err := a.SetOverride(c.key, ptr(c.value), "admin:alice"); err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
	}

	// The configuration file changes the limits (drift) and gives quay.io the alias cr.example.com,
	// clashing with the upstream created in the console. The configuration file is the baseline: that
	// upstream is suspended, the entrances that refer to it are suspended with it, unrelated overrides
	// apply as usual.
	rewrite(t, a, path, "limits:\n  manifest_rate: 3\nupstreams:\n  quay.io:\n    aliases: [cr.example.com]\n")
	rt := a.Runtime()
	if st := stateOf(a, "limits"); st.State != OverrideDrift || rt.Config.Limits.ManifestRate != 7 {
		t.Fatalf("the configuration file changed this field: the console should still win, marked drift: %+v %v", st, rt.Config.Limits.ManifestRate)
	}
	if st := stateOf(a, "upstreams/cr.example.com"); st.State != OverrideSuspended || !strings.Contains(st.Error, "cr.example.com") {
		t.Fatalf("an override that conflicts with the configuration file should be suspended: %+v", st)
	}
	// Entrances referring to the suspended upstream are suspended too; unrelated ones apply as usual
	if st := stateOf(a, "entrances"); st.State != OverrideSuspended {
		t.Fatalf("entrances that depend on the suspended upstream should be suspended too: %+v", st)
	}
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideActive || !rt.Config.Upstreams["ghcr.io"].Disabled {
		t.Fatalf("non-conflicting overrides should apply as usual: %+v", st)
	}
	if rt.Config.Entrances[0].Hosts[0] != "*" {
		t.Fatalf("entrances should come from the configuration file: %+v", rt.Config.Entrances)
	}
	if len(a.Store.Data().ConfigOverrides) != 4 {
		t.Fatal("suspended overrides should still be stored")
	}

	// Suspended overrides do not stop others from being saved
	if _, err := a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(a, "entrances"); st.State != OverrideSuspended {
		t.Fatalf("saving another override should keep the suspended one suspended: %+v", st)
	}

	// Once the conflict is resolved they resume: the upstream, then the entrances, all in one reload
	rewrite(t, a, path, "limits:\n  manifest_rate: 3\n")
	for _, k := range []string{"upstreams/cr.example.com", "entrances"} {
		if st := stateOf(a, k); !st.Applied() {
			t.Fatalf("%s should resume: %+v", k, st)
		}
	}
	var actions []string
	for _, e := range a.Store.AuditLog(50) {
		actions = append(actions, e.Action+" "+e.Target)
	}
	for _, want := range []string{"config.override.suspended entrances", "config.override.resumed entrances"} {
		if !slices.Contains(actions, want) {
			t.Fatalf("the audit log should contain %q: %v", want, actions)
		}
	}
}

func TestOverridesOnRestartAndConfigEditOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	open := func(yaml string) *App {
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
		return a
	}
	a := open("")
	_, _ = a.SetOverride("upstreams/ghcr.io", ptr("disabled: true"), "admin:alice")
	_, _ = a.SetOverride("limits", ptr("max_inflight: 50"), "admin:alice")
	a.Close()

	// On restart the configuration file conflicts with an override: start as usual and suspend it
	a = open("entrances:\n  - {hosts: [ghcr.example.com], upstream: ghcr.io}\n")
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideSuspended {
		t.Fatalf("a conflicting override should be suspended on restart: %+v", st)
	}
	if a.Runtime().Config.Limits.MaxInflight != 50 {
		t.Fatal("non-conflicting overrides should apply as usual")
	}
	// Replace the entrances that refer to ghcr.io in the console: the conflict is gone and the suspended
	// override resumes with it
	if _, err := a.SetOverride("entrances", ptr("- {hosts: ['*']}"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideActive || !a.Runtime().Config.Upstreams["ghcr.io"].Disabled {
		t.Fatalf("the suspended override should resume once the conflict is resolved: %+v", st)
	}
	a.Close()

	// admin.config_edit: false: no overrides apply, none can be saved, they can only be removed
	a = open("admin: {config_edit: false}\n")
	defer a.Close()
	if st := stateOf(a, "limits"); st.State != OverrideIgnored || a.Runtime().Config.Limits.MaxInflight == 50 {
		t.Fatalf("overrides should not apply with config_edit off: %+v", st)
	}
	if _, err := a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice"); !errors.Is(err, ErrConfigEditOff) {
		t.Fatalf("nothing should be saved with config_edit off: %v", err)
	}
	if _, err := a.SetOverride("limits", nil, "admin:alice"); err != nil || stateOf(a, "upstreams/ghcr.io").State != OverrideIgnored {
		t.Fatalf("removal should work and the rest stay ignored: %v %+v", err, a.Runtime().Overrides)
	}
}

func TestSetOverrideReadOnly(t *testing.T) {
	res, err := config.Parse([]byte("log: {access: {output: \"off\"}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(res, nil, Options{Log: slog.New(slog.DiscardHandler), Store: store.NewReadOnly()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.SetOverride("limits", ptr("max_inflight: 5"), "admin:alice"); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("read-only store: %v", err)
	}
}
