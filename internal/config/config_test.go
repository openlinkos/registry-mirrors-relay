package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		t.Fatalf("Parse 应该失败：\n%s", y)
	}
	return err.Error()
}

func TestDefaults(t *testing.T) {
	res := mustParse(t, "")
	c := res.Config
	if len(c.Upstreams) != 5 {
		t.Fatalf("内置上游应有 5 个，实际 %d", len(c.Upstreams))
	}
	e := c.Entrances[0]
	if e.Hosts[0] != "*" || e.DefaultUpstream != "docker.io" || e.Auth != "none" || e.Page != "public" {
		t.Fatalf("默认入口不对：%+v", e)
	}
	if !c.Site.ShowStatus {
		t.Fatal("用法页默认显示服务状态")
	}
	if !mustParse(t, "site: {name: x}").Config.Site.ShowStatus {
		t.Fatal("只写了 site 的其他字段时 show_status 仍默认打开")
	}
	if c.Admin.Store.Path != filepath.Join("/var/lib/registry-mirrors-relay", "state.db") {
		t.Fatalf("运营数据库路径不对：%s", c.Admin.Store.Path)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "trusted_proxies 为空") {
		t.Fatalf("应该提醒 trusted_proxies 为空：%v", res.Warnings)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	msg := parseErr(t, "listen:\n  htttp: \":8080\"\n")
	if !strings.Contains(msg, "htttp") {
		t.Fatalf("错误里应指出拼错的字段：%s", msg)
	}
	msg = parseErr(t, "upstreams:\n  quay.io:\n    redirect_host: [x]\n")
	if !strings.Contains(msg, "redirect_host") {
		t.Fatalf("上游里拼错的字段也要报错：%s", msg)
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
		t.Fatalf("覆盖 quay.io 后内置字段应保留：%+v", q)
	}
	if c.Upstreams["mcr.microsoft.com"] == nil || c.Upstreams["docker.io"] == nil {
		t.Fatal("新增上游后内置上游也应还在")
	}
	if c.Upstreams["mcr.microsoft.com"].UnlistedRedirect != "pass" {
		t.Fatal("新增上游的 unlisted_redirect 默认应是 pass")
	}
}

func TestEntranceValidation(t *testing.T) {
	cases := map[string]string{
		"未知上游": `
entrances:
  - hosts: [ghcr.example.com]
    upstream: ghcr.example.org
`,
		"重复的域名": `
entrances:
  - hosts: [a.example.com]
  - hosts: [A.example.com.]
    upstream: ghcr.io
`,
		"域名带端口": `
entrances:
  - hosts: ["a.example.com:8080"]
`,
		"同时写了两种上游": `
entrances:
  - hosts: [a.example.com]
    upstream: ghcr.io
    default_upstream: docker.io
`,
		"停用的上游": `
upstreams:
  gcr.io: {disabled: true}
entrances:
  - hosts: [gcr.example.com]
    upstream: gcr.io
`,
		"没有 hosts": `
entrances:
  - upstream: docker.io
`,
		"page 写错": `
entrances:
  - hosts: [a.example.com]
    page: hidden
`,
		"page: login 但不要求登录": `
entrances:
  - hosts: [a.example.com]
    page: login
`,
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) { parseErr(t, y) })
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
		t.Fatalf("没写 public_only 应报错：%s", msg)
	}
	res := mustParse(t, base+"      public_only: true\n")
	cr := res.Config.Upstreams["docker.io"].Credentials
	if cr.Password.Reveal() != "dckr_pat_secret" {
		t.Fatal("口令应从环境变量读出")
	}
	out, err := res.Config.Print()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "dckr_pat_secret") || !strings.Contains(string(out), "HUB_PAT") {
		t.Fatalf("打印的配置不能含明文，只显示来源：\n%s", out)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "配了账号") {
		t.Fatalf("配了账号应有提醒：%v", res.Warnings)
	}
}

func TestSecretFromFileAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("s3cret-passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := mustParse(t, "admin:\n  initial_admin:\n    username: root\n    password: {file: "+filepath.ToSlash(p)+"}\n")
	if res.Config.Admin.InitialAdmin.Password.Reveal() != "s3cret-passphrase" {
		t.Fatal("文件末尾的换行应去掉")
	}
	parseErr(t, "admin:\n  initial_admin:\n    username: root\n    password: {env: NOT_SET_ANYWHERE_42}\n")
	if got := (Secret{Value: "x"}).String(); got != redacted {
		t.Fatalf("Secret 打印应隐去：%s", got)
	}
}

func TestTrustedProxies(t *testing.T) {
	msg := parseErr(t, "client_ip:\n  trusted_proxies: [0.0.0.0/0]\n")
	if !strings.Contains(msg, "伪造") {
		t.Fatalf("信任全部地址应报错：%s", msg)
	}
	ps, err := ParsePrefixes([]string{"private_ranges", "203.0.113.7", "2001:db8::/32"})
	if err != nil || len(ps) != 8 {
		t.Fatalf("ParsePrefixes: %v %v", ps, err)
	}
	if ps[6].String() != "203.0.113.7/32" {
		t.Fatalf("单个 IP 应变成 /32：%s", ps[6])
	}
}

func TestImagePatterns(t *testing.T) {
	parseErr(t, "access:\n  images:\n    deny: [{pattern: library/nginx}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: example.org/x}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: docker.io/a**}]\n")
	parseErr(t, "access:\n  images:\n    deny: [{pattern: docker.io/x, status: 404}]\n")
	c := mustParse(t, "access:\n  images:\n    deny: [{pattern: 'ghcr.io/evil/**'}]\n    allow: ['*/library/*']\n").Config
	if c.Access.Images.Deny[0].Status != 403 {
		t.Fatal("status 默认应是 403")
	}
}

func TestUnits(t *testing.T) {
	c := mustParse(t, "cache:\n  max_bytes: 128MiB\n  digest_ttl: 7d\n  tag_ttl: 45s\nproxy:\n  buffer_size: 65536\n").Config
	if c.Cache.MaxBytes != 128<<20 || c.Cache.DigestTTL.D() != 7*24*time.Hour || c.Proxy.BufferSize != 64<<10 {
		t.Fatalf("单位解析不对：%v %v %v", c.Cache.MaxBytes, c.Cache.DigestTTL, c.Proxy.BufferSize)
	}
	if Size(64<<10).String() != "64KiB" || Size(1500).String() != "1500" {
		t.Fatal("Size.String 不对")
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
			t.Errorf("%s：期望 %v，实际 %v", host, want, got)
		}
	}
	none, _ := CompileHostPatterns(nil)
	if none.MatchString("") || none.MatchString("a") {
		t.Fatal("空白名单不应匹配任何主机")
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
		t.Fatalf("ACME 域名应取自入口：%v", c.TLS.ACME.Hosts)
	}
	parseErr(t, "listen: {https: \":443\"}\ntls: {mode: acme}\n") // 只有 * 入口，没有可申请的域名
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
		t.Fatalf("示例配置应能通过校验：%v", err)
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
		t.Fatalf("RELAY_ADMIN_PASSWORD_FILE：%+v", ia)
	}
	t.Setenv("RELAY_ADMIN_USERNAME", "ops")
	if res := mustParse(t, ""); res.Config.Admin.InitialAdmin.Username != "ops" {
		t.Fatal("RELAY_ADMIN_USERNAME")
	}
	// 配置里写了 initial_admin 时以配置为准
	t.Setenv("T_PW", "configured-password")
	if res := mustParse(t, "admin: {initial_admin: {username: root, password: {env: T_PW}}}\n"); res.Config.Admin.InitialAdmin.Username != "root" {
		t.Fatal("配置优先")
	}
	// 口令文件已经删了：只提醒，照常启动
	t.Setenv("RELAY_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "gone"))
	res = mustParse(t, "")
	if res.Config.Admin.InitialAdmin.Username != "" || !strings.Contains(strings.Join(res.Warnings, "\n"), "读不到") {
		t.Fatalf("口令文件不存在时忽略并提醒：%+v %v", res.Config.Admin.InitialAdmin, res.Warnings)
	}
	t.Setenv("RELAY_ADMIN_PASSWORD_FILE", "")
	// 口令直接写在配置里要提醒；口令等于用户名不行
	res = mustParse(t, "admin: {initial_admin: {username: root, password: {value: inline-password-1}}}\n")
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "直接写在配置里") {
		t.Fatalf("应提醒口令写在配置里：%v", res.Warnings)
	}
	parseErr(t, "admin: {initial_admin: {username: administrator1, password: {value: Administrator1}}}\n")
}

func TestRemovedKeys(t *testing.T) {
	for name, y := range map[string]string{
		"listen.metrics":     "listen:\n  http: \":8080\"\n  metrics: \":9180\"\n",
		"entrances[].listen": "entrances:\n  - listen: \":5001\"\n    upstream: docker.io\n",
	} {
		_, err := Parse([]byte(y))
		if err == nil || !strings.Contains(err.Error(), "v0.2.0") || !strings.Contains(err.Error(), "第 ") {
			t.Errorf("%s：应报错并说明改法：%v", name, err)
		}
	}
	res := mustParse(t, "listen:\n  http: \":8080\"\n  metrics: \"\"\n")
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "listen.metrics 已经去掉") {
		t.Fatalf("空的 listen.metrics 只提醒：%v", res.Warnings)
	}
}

func TestManagementSettings(t *testing.T) {
	c := mustParse(t, "").Config
	if c.Listen.Admin != "" || c.Metrics.Enabled || !c.Admin.Public || strings.Join(c.Admin.Allow, ",") != "loopback,private_ranges" || len(c.Admin.ProxiedAllow) != 0 {
		t.Fatalf("默认三合一、后台公开、指标关、指标只给本机和内网直连：%+v %+v %+v", c.Listen, c.Metrics, c.Admin)
	}
	cases := map[string]string{
		"proxied_allow 没有 public_url":      "admin: {proxied_allow: [198.51.100.0/24]}",
		"proxied_allow 没有 trusted_proxies": "admin: {public_url: \"https://a.example.com\", proxied_allow: [198.51.100.0/24]}",
		"listen.admin 和 listen.http 相同":    "listen: {admin: \":8080\"}",
		"admin.allow 写错":                   "admin: {allow: [not-an-address]}",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) { parseErr(t, y) })
	}
	mustParse(t, "client_ip: {trusted_proxies: [10.0.0.0/8]}\nadmin: {public_url: \"https://a.example.com\", proxied_allow: [198.51.100.0/24]}")
	for y, want := range map[string]string{
		"admin: {allow: [], public: false}":         "关掉了",
		"admin: {allow: []}":                        "指标和 pprof 谁也打不开",
		"listen: {http: \":8080\"}":                 "明文",
		"metrics: {pprof: true}":                    "metrics.enabled",
		"admin: {allow: [\"0.0.0.0/0\", \"::/0\"]}": "只靠口令",
	} {
		if w := strings.Join(mustParse(t, y).Warnings, "\n"); !strings.Contains(w, want) {
			t.Errorf("%s：应提醒 %q：%s", y, want, w)
		}
	}
}
