package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
)

func mustParse(t *testing.T, y string) *Result {
	t.Helper()
	res, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

func parseErr(t *testing.T, y string) string {
	t.Helper()
	_, err := Parse([]byte(y))
	if err == nil {
		t.Fatalf("Parse should fail:\n%s", y)
	}
	return err.Error()
}

func TestDefaults(t *testing.T) {
	res := mustParse(t, "")
	c := res.Config
	if len(c.Upstreams) != 5 {
		t.Fatalf("built-in upstreams = %d, want 5", len(c.Upstreams))
	}
	e := c.Entrances[0]
	if e.Hosts[0] != "*" || e.DefaultUpstream != "docker.io" || e.Auth != "none" || e.Page != "public" {
		t.Fatalf("wrong default entrance: %+v", e)
	}
	if !c.Site.ShowStatus {
		t.Fatal("the usage page should show the service status by default")
	}
	if !mustParse(t, "site: {name: x}").Config.Site.ShowStatus {
		t.Fatal("show_status should stay on by default when only other site fields are set")
	}
	if c.Admin.Store.Path != filepath.Join("/var/lib/registry-mirrors-relay", "state.db") {
		t.Fatalf("wrong operational database path: %s", c.Admin.Store.Path)
	}
	if !strings.Contains(warnText(res.Warnings), "trusted_proxies is empty") {
		t.Fatalf("want a warning that trusted_proxies is empty: %v", res.Warnings)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	msg := parseErr(t, "listen:\n  htttp: \":8080\"\n")
	if !strings.Contains(msg, "htttp") {
		t.Fatalf("the error should name the misspelt field: %s", msg)
	}
	msg = parseErr(t, "upstreams:\n  quay.io:\n    redirect_host: [x]\n")
	if !strings.Contains(msg, "redirect_host") {
		t.Fatalf("a misspelt field in an upstream should be an error too: %s", msg)
	}
}

func TestUpstreamOverrideKeepsBuiltin(t *testing.T) {
	c := mustParse(t, `
upstreams:
  quay.io:
    unlisted_redirect: deny
    extra_redirect_hosts: ['cdn99\.example\.net']
  mcr.microsoft.com:
    registry: mcr.microsoft.com
    redirect_hosts: ['(?:[a-z0-9-]+\.)*mcr\.microsoft\.com']
    token_realm_hosts: ['mcr\.microsoft\.com']
`).Config
	q := c.Upstreams["quay.io"]
	if q.Registry != "quay.io" || len(q.RedirectHosts) != 3 || q.UnlistedRedirect != "deny" || len(q.ExtraRedirectHosts) != 1 {
		t.Fatalf("built-in fields should be kept after overriding quay.io: %+v", q)
	}
	if c.Upstreams["mcr.microsoft.com"] == nil || c.Upstreams["docker.io"] == nil {
		t.Fatal("built-in upstreams should remain after adding an upstream")
	}
	if c.Upstreams["mcr.microsoft.com"].UnlistedRedirect != "pass" {
		t.Fatal("unlisted_redirect of a new upstream should default to pass")
	}
}

func TestEntranceValidation(t *testing.T) {
	cases := map[string]string{
		"unknown upstream": `
entrances:
  - hosts: [ghcr.example.com]
    upstream: ghcr.example.org
`,
		"duplicate domain": `
entrances:
  - hosts: [a.example.com]
  - hosts: [A.example.com.]
    upstream: ghcr.io
`,
		"domain with port": `
entrances:
  - hosts: ["a.example.com:8080"]
`,
		"both kinds of upstream": `
entrances:
  - hosts: [a.example.com]
    upstream: ghcr.io
    default_upstream: docker.io
`,
		"disabled upstream": `
upstreams:
  gcr.io: {disabled: true}
entrances:
  - hosts: [gcr.example.com]
    upstream: gcr.io
`,
		"no hosts": `
entrances:
  - upstream: docker.io
`,
		"invalid page": `
entrances:
  - hosts: [a.example.com]
    page: hidden
`,
		"page: login but no sign-in required": `
entrances:
  - hosts: [a.example.com]
    page: login
`,
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) { parseErr(t, y) })
	}
}

func TestSiteBrand(t *testing.T) {
	if c := mustParse(t, "site:\n  brand: linkos-mirrors\n").Config; c.Site.Brand != "linkos-mirrors" {
		t.Errorf("brand = %q", c.Site.Brand)
	}
	if msg := parseErr(t, "site:\n  brand: nope\n"); !strings.Contains(msg, `site.brand "nope"`) || !strings.Contains(msg, "linkos-mirrors") {
		t.Errorf("an unknown brand should be rejected and the brands listed: %s", msg)
	}
}

func TestCredentialsRequirePublicOnly(t *testing.T) {
	t.Setenv("HUB_PAT", "dckr_pat_secret")
	base := `
upstreams:
  docker.io:
    credentials:
      username: bot
      password: {env: HUB_PAT}
      realm_host: auth.docker.io
`
	if msg := parseErr(t, base); !strings.Contains(msg, "public_only") {
		t.Fatalf("a missing public_only should be an error: %s", msg)
	}
	res := mustParse(t, base+"      public_only: true\n")
	cr := res.Config.Upstreams["docker.io"].Credentials
	if cr.Password.Reveal() != "dckr_pat_secret" {
		t.Fatal("the password should be read from the environment variable")
	}
	out, err := res.Config.Print()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "dckr_pat_secret") || !strings.Contains(string(out), "HUB_PAT") {
		t.Fatalf("the printed configuration must not contain the plain text, only the source:\n%s", out)
	}
	if !strings.Contains(warnText(res.Warnings), "has credentials") {
		t.Fatalf("want a warning about the configured account: %v", res.Warnings)
	}
}

func TestSecretFromFileAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("s3cret-passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := mustParse(t, "admin:\n  initial_admin:\n    username: root\n    password: {file: "+filepath.ToSlash(p)+"}\n")
	if res.Config.Admin.InitialAdmin.Password.Reveal() != "s3cret-passphrase" {
		t.Fatal("the trailing newline in the file should be removed")
	}
	parseErr(t, "admin:\n  initial_admin:\n    username: root\n    password: {env: NOT_SET_ANYWHERE_42}\n")
	if got := (Secret{Value: "x"}).String(); got != redacted {
		t.Fatalf("a printed Secret should be redacted: %s", got)
	}
}

func TestTrustedProxies(t *testing.T) {
	msg := parseErr(t, "client_ip:\n  trusted_proxies: [0.0.0.0/0]\n")
	if !strings.Contains(msg, "forged") {
		t.Fatalf("trusting every address should be an error: %s", msg)
	}
	ps, err := ParsePrefixes([]string{"private_ranges", "203.0.113.7", "2001:db8::/32"})
	if err != nil || len(ps) != 8 {
		t.Fatalf("ParsePrefixes: %v %v", ps, err)
	}
	if ps[6].String() != "203.0.113.7/32" {
		t.Fatalf("a single IP should become /32: %s", ps[6])
	}
}

func TestImagePatterns(t *testing.T) {
	parseErr(t, "access:\n  images:\n    deny: [{pattern: library/nginx}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: example.org/x}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: docker.io/a**}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: docker.io/x, status: 404}]\n")
	c := mustParse(t, "access:\n  images:\n    deny: [{pattern: 'ghcr.io/evil/**'}]\n    allow: ['*/library/*']\n").Config
	if c.Access.Images.Deny[0].Status != 403 {
		t.Fatal("status should default to 403")
	}
}

func TestUnits(t *testing.T) {
	c := mustParse(t, "cache:\n  max_bytes: 128MiB\n  digest_ttl: 7d\n  tag_ttl: 45s\nproxy:\n  buffer_size: 65536\n").Config
	if c.Cache.MaxBytes != 128<<20 || c.Cache.DigestTTL.D() != 7*24*time.Hour || c.Proxy.BufferSize != 64<<10 {
		t.Fatalf("wrong unit parsing: %v %v %v", c.Cache.MaxBytes, c.Cache.DigestTTL, c.Proxy.BufferSize)
	}
	if Size(64<<10).String() != "64KiB" || Size(1500).String() != "1500" {
		t.Fatal("wrong Size.String")
	}
	parseErr(t, "cache:\n  tag_ttl: -5s\n")
	parseErr(t, "cache:\n  max_bytes: 10XB\n")
}

func TestCompileHostPatternsIsAnchored(t *testing.T) {
	re, err := CompileHostPatterns(BuiltinUpstreams()["docker.io"].RedirectHosts)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"production.cloudfront.docker.com":                                             true,
		"PRODUCTION.CLOUDFRONT.DOCKER.COM":                                             true,
		"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com": true,
		"evil.docker.com.attacker.net":                                                 false,
		"docker.com":                                                                   false,
		"registry-1.docker.io.evil":                                                    false,
	} {
		if got := re.MatchString(host); got != want {
			t.Errorf("%s: want %v, got %v", host, want, got)
		}
	}
	none, _ := CompileHostPatterns(nil)
	if none.MatchString("") || none.MatchString("a") {
		t.Fatal("an empty allowlist should match no host")
	}
}

func TestACMEHostsFromEntrances(t *testing.T) {
	c := mustParse(t, `
listen: {http: ":80", https: ":443"}
tls: {mode: acme}
entrances:
  - hosts: [mirror.example.com]
  - hosts: [ghcr.example.com]
    upstream: ghcr.io
`).Config
	if strings.Join(c.TLS.ACME.Hosts, ",") != "ghcr.example.com,mirror.example.com" {
		t.Fatalf("ACME domains should come from the entrances: %v", c.TLS.ACME.Hosts)
	}
	parseErr(t, "listen: {https: \":443\"}\ntls: {mode: acme}\n") // only a * entrance, so no domain to obtain a certificate for
}

func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKERHUB_TOKEN", "x")
	t.Setenv("RELAY_ADMIN_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("RELAY_ADMIN_PASSWORD", "example-admin-password")
	if _, err := Parse(b); err != nil {
		t.Fatalf("the example configuration should pass validation: %v", err)
	}
}

func TestInitialAdminFromEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("from-secret-file-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_ADMIN_PASSWORD_FILE", p)
	res := mustParse(t, "")
	if ia := res.Config.Admin.InitialAdmin; ia.Username != "admin" || ia.Password.Reveal() != "from-secret-file-1" || !ia.FromEnv {
		t.Fatalf("RELAY_ADMIN_PASSWORD_FILE: %+v", ia)
	}
	t.Setenv("RELAY_ADMIN_USERNAME", "ops")
	if res := mustParse(t, ""); res.Config.Admin.InitialAdmin.Username != "ops" {
		t.Fatal("RELAY_ADMIN_USERNAME")
	}
	// initial_admin in the configuration wins
	t.Setenv("T_PW", "configured-password")
	if res := mustParse(t, "admin: {initial_admin: {username: root, password: {env: T_PW}}}\n"); res.Config.Admin.InitialAdmin.Username != "root" {
		t.Fatal("the configuration should take precedence")
	}
	// the password file has been deleted: only a warning, and startup goes ahead
	t.Setenv("RELAY_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "gone"))
	res = mustParse(t, "")
	if res.Config.Admin.InitialAdmin.Username != "" || !strings.Contains(warnText(res.Warnings), "cannot be read") {
		t.Fatalf("a missing password file should be ignored with a warning: %+v %v", res.Config.Admin.InitialAdmin, res.Warnings)
	}
	t.Setenv("RELAY_ADMIN_PASSWORD_FILE", "")
	// an inline password gives a warning; a password equal to the user name is refused
	res = mustParse(t, "admin: {initial_admin: {username: root, password: {value: inline-password-1}}}\n")
	if !strings.Contains(warnText(res.Warnings), "written in the configuration") {
		t.Fatalf("want a warning about the inline password: %v", res.Warnings)
	}
	parseErr(t, "admin: {initial_admin: {username: administrator1, password: {value: Administrator1}}}\n")
}

func TestRemovedKeys(t *testing.T) {
	for name, y := range map[string]string{
		"listen.metrics":     "listen:\n  http: \":8080\"\n  metrics: \":9180\"\n",
		"entrances[].listen": "entrances:\n  - listen: \":5001\"\n    upstream: docker.io\n",
	} {
		_, err := Parse([]byte(y))
		if err == nil || !strings.Contains(err.Error(), "v0.2.0") || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: want an error that explains what to write instead: %v", name, err)
		}
	}
	res := mustParse(t, "listen:\n  http: \":8080\"\n  metrics: \"\"\n")
	if !strings.Contains(warnText(res.Warnings), "listen.metrics was removed") {
		t.Fatalf("an empty listen.metrics should only give a warning: %v", res.Warnings)
	}
}

func TestManagementSettings(t *testing.T) {
	c := mustParse(t, "").Config
	if c.Listen.Admin != "" || c.Metrics.Enabled || !c.Admin.Public || strings.Join(c.Admin.Allow, ",") != "loopback,private_ranges" || len(c.Admin.ProxiedAllow) != 0 {
		t.Fatalf("want defaults: one port for everything, public console, metrics off and only for direct local and private connections: %+v %+v %+v", c.Listen, c.Metrics, c.Admin)
	}
	cases := map[string]string{
		"proxied_allow without public_url":      "admin: {proxied_allow: [198.51.100.0/24]}",
		"proxied_allow without trusted_proxies": "admin: {public_url: \"https://a.example.com\", proxied_allow: [198.51.100.0/24]}",
		"listen.admin same as listen.http":      "listen: {admin: \":8080\"}",
		"invalid admin.allow":                   "admin: {allow: [not-an-address]}",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) { parseErr(t, y) })
	}
	mustParse(t, "client_ip: {trusted_proxies: [10.0.0.0/8]}\nadmin: {public_url: \"https://a.example.com\", proxied_allow: [198.51.100.0/24]}")
	for y, want := range map[string]string{
		"admin: {allow: [], public: false}":         "closed to everyone",
		"admin: {allow: []}":                        "nobody can reach metrics",
		"listen: {http: \":8080\"}":                 "clear text",
		"metrics: {pprof: true}":                    "metrics.enabled",
		"admin: {allow: [\"0.0.0.0/0\", \"::/0\"]}": "only passwords",
	} {
		if w := warnText(mustParse(t, y).Warnings); !strings.Contains(w, want) {
			t.Errorf("%s: want a warning containing %q: %s", y, want, w)
		}
	}
}

func warnText(ws []i18n.Message) string {
	var out []string
	for _, w := range ws {
		out = append(out, w.String())
	}
	return strings.Join(out, "\n")
}
