package config

import (
	"strings"
	"testing"
	"time"
)

func parseOv(t *testing.T, file string, ovs ...Override) *Config {
	t.Helper()
	res, err := ParseWithOverrides([]byte(file), ovs)
	if err != nil {
		t.Fatalf("ParseWithOverrides: %v", err)
	}
	return res.Config
}

func parseOvErr(t *testing.T, file string, ovs ...Override) string {
	t.Helper()
	_, err := ParseWithOverrides([]byte(file), ovs)
	if err == nil {
		t.Fatalf("should fail: %+v", ovs)
	}
	return err.Error()
}

func TestOverrideFieldLevel(t *testing.T) {
	file := "limits:\n  manifest_rate: 5\n  manifest_burst: 10\ncache:\n  tag_ttl: 1m\n"
	c := parseOv(t, file,
		Override{"limits", "manifest_burst: 20\nexempt_cidrs: [10.0.0.0/8]"},
		Override{"cache", `{"digest_ttl": "2h"}`}, // JSON works too
		Override{"site", "show_status: false"},
		Override{"proxy", "health_interval: 30s"})
	if c.Limits.ManifestRate != 5 || c.Limits.ManifestBurst != 20 || len(c.Limits.ExemptCIDRs) != 1 {
		t.Fatalf("limits should be overridden field by field: %+v", c.Limits)
	}
	if c.Cache.TagTTL.D() != time.Minute || c.Cache.DigestTTL.D() != 2*time.Hour {
		t.Fatalf("cache should be overridden field by field: %+v", c.Cache)
	}
	if c.Site.ShowStatus || c.Proxy.HealthInterval.D() != 30*time.Second {
		t.Fatalf("site, proxy: %+v %+v", c.Site, c.Proxy)
	}
	// with an empty configuration file, overrides apply on top of the defaults; a section that is null in the
	// configuration file can be changed too
	if c := parseOv(t, "", Override{"limits", "max_inflight: 7"}); c.Limits.MaxInflight != 7 || c.Limits.ManifestBurst != Defaults().Limits.ManifestBurst {
		t.Fatalf("empty configuration: %+v", c.Limits)
	}
	if c := parseOv(t, "limits:\n", Override{"limits", "max_inflight: 7"}); c.Limits.MaxInflight != 7 {
		t.Fatalf("null section: %+v", c.Limits)
	}
}

func TestOverrideRejected(t *testing.T) {
	for _, c := range []struct {
		o    Override
		want string
	}{
		{Override{"listen", "http: :80"}, "cannot be changed in the console"},
		{Override{"admin", "allow: [0.0.0.0/0]"}, "cannot be changed in the console"},
		{Override{"upstreams", "x: {}"}, "cannot be changed in the console"},
		{Override{"upstreams/NotAName", "registry: a.example.com"}, "cannot be changed in the console"},
		{Override{"limits", "tracked_clients: 10"}, "only in the configuration file"},
		{Override{"proxy", "buffer_size: 1MiB"}, "only in the configuration file"},
		{Override{"upstreams/docker.io", "allow_private_network: true"}, "only in the configuration file"},
		{Override{"upstreams/docker.io", "credentials: {username: x}"}, "only in the configuration file"},
		{Override{"entrances", "- {hosts: [a.example.com], listen: ':5000'}"}, "only in the configuration file"},
		{Override{"entrances", "hosts: [a.example.com]"}, "must be a list"},
		{Override{"limits", "- 1"}, "must be a mapping"},
		{Override{"limits", "manifest_rate: abc"}, "abc"},
		{Override{"cache", "tag_ttl: forever"}, "forever"},
		{Override{"limits", ""}, "empty"},
		{Override{"limits", "{"}, "YAML"},
	} {
		if err := CheckOverride(c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: want an error containing %q, got %v", c.o, c.want, err)
		}
		if msg := parseOvErr(t, "", c.o); !strings.Contains(msg, c.o.Key) {
			t.Errorf("the error from a full parse should name the override: %s", msg)
		}
	}
	// invalid only when combined: validated just as when editing the configuration file
	msg := parseOvErr(t, "", Override{"entrances", "- {hosts: [a.example.com], upstream: nope.example.com}"})
	if !strings.Contains(msg, "nope.example.com") {
		t.Fatalf("entrance referring to a missing upstream: %s", msg)
	}
}

func TestOverrideUpstreamsAndEntrances(t *testing.T) {
	file := "entrances:\n  - {hosts: [mirror.example.com]}\nupstreams:\n  ghcr.io:\n    extra_redirect_hosts: [a.example.com]\n"
	c := parseOv(t, file,
		Override{"upstreams/ghcr.io", "disabled: true"},
		Override{"upstreams/cr.example.com", "registry: registry.example.com\ntoken_realm_hosts: [auth.example.com]"},
		Override{"entrances", "- {hosts: [mirror.example.com]}\n- {name: own, hosts: [CR.example.com], upstream: cr.example.com, page: off}\n"})
	if g := c.Upstreams["ghcr.io"]; !g.Disabled || len(g.ExtraRedirectHosts) != 1 || g.Registry != "ghcr.io" {
		t.Fatalf("the upstream should be overridden field by field, keeping built-in fields: %+v", g)
	}
	if u := c.Upstreams["cr.example.com"]; u == nil || u.Registry != "registry.example.com" || u.UnlistedRedirect != "pass" {
		t.Fatalf("want a new upstream with defaults filled in: %+v", u)
	}
	if len(c.Entrances) != 2 || c.Entrances[1].Hosts[0] != "cr.example.com" || c.Entrances[1].Page != "off" || c.Entrances[1].Name != "own" {
		t.Fatalf("the entrance list should be replaced as a whole: %+v", c.Entrances)
	}
	if len(c.Upstreams) != 6 {
		t.Fatalf("the built-in upstreams should remain: %d", len(c.Upstreams))
	}
}

func TestOverrideCredentialLock(t *testing.T) {
	t.Setenv("HUB_PAT", "dckr_pat_secret")
	file := `
upstreams:
  Docker.io:
    credentials:
      username: bot
      password: {env: HUB_PAT}
      realm_host: auth.docker.io
      public_only: true
`
	for _, v := range []string{"registry: evil.example.com", "token_realm_hosts: [evil.example.com]"} {
		if msg := parseOvErr(t, file, Override{"upstreams/docker.io", v}); !strings.Contains(msg, "has credentials") {
			t.Fatalf("an upstream with an account must not allow changing %s: %s", v, msg)
		}
	}
	c := parseOv(t, file, Override{"upstreams/docker.io", "extra_redirect_hosts: [cdn.example.com]"})
	if u := c.Upstreams["docker.io"]; u.Credentials == nil || len(u.ExtraRedirectHosts) != 1 {
		t.Fatalf("other fields can be changed and the account is kept: %+v", u)
	}
}

func TestMinimizeAndBase(t *testing.T) {
	file := mustParse(t, "limits:\n  manifest_rate: 5\ncache:\n  tag_ttl: 10m\n").Config
	o, keep, err := MinimizeOverride(file, Override{"cache", "tag_ttl: 600s\ndigest_ttl: 1h"})
	if err != nil || !keep || strings.Contains(o.Value, "tag_ttl") || !strings.Contains(o.Value, "digest_ttl") {
		t.Fatalf("fields equal to the configuration file should be removed: %q %v %v", o.Value, keep, err)
	}
	if _, keep, _ := MinimizeOverride(file, Override{"limits", "manifest_rate: 5.0\nexempt_cidrs: []"}); keep {
		t.Fatal("an override equal to the configuration file in every field can be removed")
	}
	if _, keep, _ := MinimizeOverride(file, Override{"entrances", "- {name: default, hosts: ['*']}"}); keep {
		t.Fatal("an entrance list equal to the configuration file (the defaults) can be removed")
	}
	if _, keep, _ := MinimizeOverride(file, Override{"upstreams/new.example.com", "registry: new.example.com"}); !keep {
		t.Fatal("a new upstream should be kept")
	}
	if _, _, err := MinimizeOverride(file, Override{"limits", "tracked_clients: 1"}); err == nil {
		t.Fatal("a field that cannot be changed should be an error")
	}

	ov := Override{"limits", "manifest_rate: 9"}
	b0 := OverrideBase(file, ov)
	if b0 == "" || OverrideBase(mustParse(t, "limits:\n  manifest_rate: 5\n  max_inflight: 3\n").Config, ov) != b0 {
		t.Fatal("the fingerprint should stay the same when a field the override does not change changes")
	}
	if OverrideBase(mustParse(t, "limits:\n  manifest_rate: 6\n").Config, ov) == b0 {
		t.Fatal("the fingerprint should change when the overridden field changes in the configuration file")
	}
	eo := Override{"entrances", "- {hosts: [a.example.com]}"}
	if OverrideBase(file, eo) == OverrideBase(mustParse(t, "entrances:\n  - {hosts: [b.example.com]}\n").Config, eo) {
		t.Fatal("the fingerprint should change when the entrance list changes")
	}
}

func TestFormOverride(t *testing.T) {
	o, err := FormOverride("limits", map[string]string{"manifest_rate": "2.5", "max_inflight": "100", "exempt_cidrs": "10.0.0.0/8, 192.168.0.0/16\n"})
	if err != nil {
		t.Fatal(err)
	}
	c := parseOv(t, "", o)
	if c.Limits.ManifestRate != 2.5 || c.Limits.MaxInflight != 100 || len(c.Limits.ExemptCIDRs) != 2 {
		t.Fatalf("%+v", c.Limits)
	}
	if got := splitList("a.example.com,b.example.com  c.example.com,\n cdn[0-9]{1,3}\\.example\\.com, ,"); len(got) != 4 || got[3] != `cdn[0-9]{1,3}\.example\.com` {
		t.Fatalf("lists should split at whitespace and commas, but not inside {1,3} in a regex: %q", got)
	}
	if _, err := FormOverride("limits", map[string]string{"max_inflight": "1.5"}); err == nil {
		t.Fatal("a decimal in an integer field should be an error")
	}
	if _, err := FormOverride("listen", map[string]string{"http": ":80"}); err == nil {
		t.Fatal("a section that cannot be changed should be an error")
	}
	o, _ = FormOverride("upstreams/ghcr.io", map[string]string{"disabled": "true", "library_prefix": ""})
	if c := parseOv(t, "", o); !c.Upstreams["ghcr.io"].Disabled {
		t.Fatalf("%q", o.Value)
	}
	o, err = FormEntrances([]map[string]string{{"hosts": "a.example.com b.example.com", "auth": ""}, {"hosts": "q.example.com", "upstream": "quay.io"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := parseOv(t, "", o); len(c.Entrances) != 2 || len(c.Entrances[0].Hosts) != 2 || c.Entrances[0].Auth != "none" || c.Entrances[1].Upstream != "quay.io" {
		t.Fatalf("%+v", c.Entrances)
	}

	file := mustParse(t, "cache:\n  tag_ttl: 1h\n  digest_ttl: 7d\nlimits:\n  exempt_cidrs: [10.0.0.0/8, 172.16.0.0/12]\n").Config
	for _, c := range []struct{ key, field, want string }{
		{"cache", "tag_ttl", "1h"}, {"cache", "digest_ttl", "7d"}, {"cache", "max_bytes", "64MiB"},
		{"limits", "exempt_cidrs", "10.0.0.0/8, 172.16.0.0/12"}, {"limits", "head_weight", "0.25"},
		{"upstreams/docker.io", "library_prefix", "true"}, {"upstreams/none.example.com", "registry", ""},
	} {
		if got := FieldText(file, c.key, c.field); got != c.want {
			t.Errorf("FieldText(%s, %s) = %q, want %q", c.key, c.field, got, c.want)
		}
	}
	for _, c := range []struct{ key, field, want string }{
		{"cache", "tag_ttl", "duration"}, {"cache", "max_bytes", "size"}, {"limits", "max_inflight", "int"},
		{"limits", "manifest_rate", "float"}, {"site", "show_status", "bool"}, {"entrances", "hosts", "list"},
		{"upstreams/x.example.com", "registry", "string"}, {"limits", "nope", ""},
	} {
		if got := FieldKind(c.key, c.field); got != c.want {
			t.Errorf("FieldKind(%s, %s) = %q, want %q", c.key, c.field, got, c.want)
		}
	}
}

func TestExport(t *testing.T) {
	ovs := []Override{{"limits", "max_inflight: 7"}, {"upstreams/ghcr.io", "disabled: true"}, {"entrances", "- {hosts: [a.example.com]}"}}
	b, err := ExportOverrides(ovs)
	if err != nil {
		t.Fatal(err)
	}
	c := parseOv(t, string(b))
	if c.Limits.MaxInflight != 7 || !c.Upstreams["ghcr.io"].Disabled || c.Entrances[0].Hosts[0] != "a.example.com" {
		t.Fatalf("the exported overrides should work as a configuration as they are:\n%s", b)
	}
	if b, _ := ExportOverrides(nil); len(b) != 0 {
		t.Fatalf("the export should be empty without overrides: %q", b)
	}

	file := `# Site
limits:
  max_inflight: 3 # original value
admin:
  initial_admin:
    username: admin
    password: {value: plain-text-password-1}
  api_tokens:
    - {name: ci, token: {value: 0123456789abcdef0123456789abcdef}}
`
	b, err = ExportMerged([]byte(file), ovs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "plain-text-password-1") || strings.Contains(s, "0123456789abcdef") || !strings.Contains(s, "fill in the original value") {
		t.Fatalf("inline secrets should be redacted:\n%s", s)
	}
	if !strings.Contains(s, "# Site") || !strings.Contains(s, "max_inflight: 7") || !strings.Contains(s, "disabled: true") {
		t.Fatalf("comments should be kept and the overrides merged in:\n%s", s)
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("check should fail when the redacted secrets are not filled back in")
	}
}
