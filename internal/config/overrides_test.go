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
		t.Fatalf("应该失败：%+v", ovs)
	}
	return err.Error()
}

func TestOverrideFieldLevel(t *testing.T) {
	file := "limits:\n  manifest_rate: 5\n  manifest_burst: 10\ncache:\n  tag_ttl: 1m\n"
	c := parseOv(t, file,
		Override{"limits", "manifest_burst: 20\nexempt_cidrs: [10.0.0.0/8]"},
		Override{"cache", `{"digest_ttl": "2h"}`}, // JSON 也行
		Override{"site", "show_status: false"},
		Override{"proxy", "health_interval: 30s"})
	if c.Limits.ManifestRate != 5 || c.Limits.ManifestBurst != 20 || len(c.Limits.ExemptCIDRs) != 1 {
		t.Fatalf("limits 应逐字段覆盖：%+v", c.Limits)
	}
	if c.Cache.TagTTL.D() != time.Minute || c.Cache.DigestTTL.D() != 2*time.Hour {
		t.Fatalf("cache 应逐字段覆盖：%+v", c.Cache)
	}
	if c.Site.ShowStatus || c.Proxy.HealthInterval.D() != 30*time.Second {
		t.Fatalf("site、proxy：%+v %+v", c.Site, c.Proxy)
	}
	// 配置文件为空时叠加在默认值上；配置文件里是空值的段也能改
	if c := parseOv(t, "", Override{"limits", "max_inflight: 7"}); c.Limits.MaxInflight != 7 || c.Limits.ManifestBurst != Defaults().Limits.ManifestBurst {
		t.Fatalf("空配置：%+v", c.Limits)
	}
	if c := parseOv(t, "limits:\n", Override{"limits", "max_inflight: 7"}); c.Limits.MaxInflight != 7 {
		t.Fatalf("空值的段：%+v", c.Limits)
	}
}

func TestOverrideRejected(t *testing.T) {
	for _, c := range []struct {
		o    Override
		want string
	}{
		{Override{"listen", "http: :80"}, "后台不能改"},
		{Override{"admin", "allow: [0.0.0.0/0]"}, "后台不能改"},
		{Override{"upstreams", "x: {}"}, "后台不能改"},
		{Override{"upstreams/NotAName", "registry: a.example.com"}, "后台不能改"},
		{Override{"limits", "tracked_clients: 10"}, "只能在配置文件里改"},
		{Override{"proxy", "buffer_size: 1MiB"}, "只能在配置文件里改"},
		{Override{"upstreams/docker.io", "allow_private_network: true"}, "只能在配置文件里改"},
		{Override{"upstreams/docker.io", "credentials: {username: x}"}, "只能在配置文件里改"},
		{Override{"entrances", "- {hosts: [a.example.com], listen: ':5000'}"}, "只能在配置文件里改"},
		{Override{"entrances", "hosts: [a.example.com]"}, "应该是列表"},
		{Override{"limits", "- 1"}, "应该是键值对"},
		{Override{"limits", "manifest_rate: abc"}, "abc"},
		{Override{"cache", "tag_ttl: forever"}, "forever"},
		{Override{"limits", ""}, "空的"},
		{Override{"limits", "{"}, "YAML"},
	} {
		if err := CheckOverride(c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v：应报错含 %q，实际 %v", c.o, c.want, err)
		}
		if msg := parseOvErr(t, "", c.o); !strings.Contains(msg, c.o.Key) {
			t.Errorf("整体解析的错误应指出是哪一处修改：%s", msg)
		}
	}
	// 合起来不对：校验和改配置文件时一样
	msg := parseOvErr(t, "", Override{"entrances", "- {hosts: [a.example.com], upstream: nope.example.com}"})
	if !strings.Contains(msg, "nope.example.com") {
		t.Fatalf("入口引用不存在的上游：%s", msg)
	}
}

func TestOverrideUpstreamsAndEntrances(t *testing.T) {
	file := "entrances:\n  - {hosts: [mirror.example.com]}\nupstreams:\n  ghcr.io:\n    extra_redirect_hosts: [a.example.com]\n"
	c := parseOv(t, file,
		Override{"upstreams/ghcr.io", "disabled: true"},
		Override{"upstreams/cr.example.com", "registry: registry.example.com\ntoken_realm_hosts: [auth.example.com]"},
		Override{"entrances", "- {hosts: [mirror.example.com]}\n- {name: own, hosts: [CR.example.com], upstream: cr.example.com, page: off}\n"})
	if g := c.Upstreams["ghcr.io"]; !g.Disabled || len(g.ExtraRedirectHosts) != 1 || g.Registry != "ghcr.io" {
		t.Fatalf("上游逐字段覆盖，内置的字段保留：%+v", g)
	}
	if u := c.Upstreams["cr.example.com"]; u == nil || u.Registry != "registry.example.com" || u.UnlistedRedirect != "pass" {
		t.Fatalf("应新建上游并补默认值：%+v", u)
	}
	if len(c.Entrances) != 2 || c.Entrances[1].Hosts[0] != "cr.example.com" || c.Entrances[1].Page != "off" || c.Entrances[1].Name != "own" {
		t.Fatalf("入口列表整体替换：%+v", c.Entrances)
	}
	if len(c.Upstreams) != 6 {
		t.Fatalf("内置上游还在：%d", len(c.Upstreams))
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
		if msg := parseOvErr(t, file, Override{"upstreams/docker.io", v}); !strings.Contains(msg, "配了账号") {
			t.Fatalf("配了账号的上游不能改 %s：%s", v, msg)
		}
	}
	c := parseOv(t, file, Override{"upstreams/docker.io", "extra_redirect_hosts: [cdn.example.com]"})
	if u := c.Upstreams["docker.io"]; u.Credentials == nil || len(u.ExtraRedirectHosts) != 1 {
		t.Fatalf("其他字段可以改，账号保留：%+v", u)
	}
}

func TestMinimizeAndBase(t *testing.T) {
	file := mustParse(t, "limits:\n  manifest_rate: 5\ncache:\n  tag_ttl: 10m\n").Config
	o, keep, err := MinimizeOverride(file, Override{"cache", "tag_ttl: 600s\ndigest_ttl: 1h"})
	if err != nil || !keep || strings.Contains(o.Value, "tag_ttl") || !strings.Contains(o.Value, "digest_ttl") {
		t.Fatalf("和配置文件一样的字段去掉：%q %v %v", o.Value, keep, err)
	}
	if _, keep, _ := MinimizeOverride(file, Override{"limits", "manifest_rate: 5.0\nexempt_cidrs: []"}); keep {
		t.Fatal("全都和配置文件一样时可以去掉")
	}
	if _, keep, _ := MinimizeOverride(file, Override{"entrances", "- {name: default, hosts: ['*']}"}); keep {
		t.Fatal("入口列表和配置文件（默认值）一样时可以去掉")
	}
	if _, keep, _ := MinimizeOverride(file, Override{"upstreams/new.example.com", "registry: new.example.com"}); !keep {
		t.Fatal("新建的上游要保留")
	}
	if _, _, err := MinimizeOverride(file, Override{"limits", "tracked_clients: 1"}); err == nil {
		t.Fatal("不能改的字段要报错")
	}

	ov := Override{"limits", "manifest_rate: 9"}
	b0 := OverrideBase(file, ov)
	if b0 == "" || OverrideBase(mustParse(t, "limits:\n  manifest_rate: 5\n  max_inflight: 3\n").Config, ov) != b0 {
		t.Fatal("没改的字段变了，指纹不变")
	}
	if OverrideBase(mustParse(t, "limits:\n  manifest_rate: 6\n").Config, ov) == b0 {
		t.Fatal("改的字段在配置文件里变了，指纹要变")
	}
	eo := Override{"entrances", "- {hosts: [a.example.com]}"}
	if OverrideBase(file, eo) == OverrideBase(mustParse(t, "entrances:\n  - {hosts: [b.example.com]}\n").Config, eo) {
		t.Fatal("入口列表变了，指纹要变")
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
		t.Fatalf("列表按空白和逗号分，正则里的 {1,3} 不分：%q", got)
	}
	if _, err := FormOverride("limits", map[string]string{"max_inflight": "1.5"}); err == nil {
		t.Fatal("整数字段填小数要报错")
	}
	if _, err := FormOverride("listen", map[string]string{"http": ":80"}); err == nil {
		t.Fatal("不能改的段")
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
			t.Errorf("FieldText(%s, %s) = %q，应为 %q", c.key, c.field, got, c.want)
		}
	}
	for _, c := range []struct{ key, field, want string }{
		{"cache", "tag_ttl", "duration"}, {"cache", "max_bytes", "size"}, {"limits", "max_inflight", "int"},
		{"limits", "manifest_rate", "float"}, {"site", "show_status", "bool"}, {"entrances", "hosts", "list"},
		{"upstreams/x.example.com", "registry", "string"}, {"limits", "nope", ""},
	} {
		if got := FieldKind(c.key, c.field); got != c.want {
			t.Errorf("FieldKind(%s, %s) = %q，应为 %q", c.key, c.field, got, c.want)
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
		t.Fatalf("导出的片段应能直接当配置用：\n%s", b)
	}
	if b, _ := ExportOverrides(nil); len(b) != 0 {
		t.Fatalf("没有修改时导出为空：%q", b)
	}

	file := `# 站点
limits:
  max_inflight: 3 # 原来的值
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
	if strings.Contains(s, "plain-text-password-1") || strings.Contains(s, "0123456789abcdef") || !strings.Contains(s, "请填回") {
		t.Fatalf("直接写的密钥要隐去：\n%s", s)
	}
	if !strings.Contains(s, "# 站点") || !strings.Contains(s, "max_inflight: 7") || !strings.Contains(s, "disabled: true") {
		t.Fatalf("保留注释并合进修改：\n%s", s)
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("隐去密钥后不填回，check 应报错")
	}
}
