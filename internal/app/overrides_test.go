package app

import (
	"encoding/json"
	"errors"
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

// rewrite 改配置文件并重新加载。
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
		t.Fatalf("生效的是叠加后的，FileConfig 是配置文件的：%+v %+v", rt.Config.Limits, rt.FileConfig.Limits)
	}
	if rt.Pool != first.Pool {
		t.Fatal("只改限流，连接池应沿用")
	}
	saved := a.Store.Data().ConfigOverrides["limits"]
	if saved.UpdatedBy != "admin:alice" || saved.Base == "" || !strings.Contains(saved.Value, "manifest_burst") {
		t.Fatalf("存下的修改：%+v", saved)
	}
	if st := stateOf(a, "limits"); st.State != OverrideActive {
		t.Fatalf("状态：%+v", st)
	}

	// 和配置文件一样的字段不存；全都一样时等于去掉
	if _, err := a.SetOverride("limits", ptr("manifest_rate: 2\nmanifest_burst: 20"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if v := a.Store.Data().ConfigOverrides["limits"].Value; strings.Contains(v, "manifest_rate") {
		t.Fatalf("和配置文件一样的字段不存：%q", v)
	}
	if _, err := a.SetOverride("limits", ptr("manifest_rate: 2"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Store.Data().ConfigOverrides["limits"]; ok || len(a.Runtime().Overrides) != 0 {
		t.Fatal("全都和配置文件一样时去掉这一处")
	}

	// 改上游的连接设置：新的连接池
	before := a.Runtime()
	if _, err := a.SetOverride("proxy", ptr("max_redirects: 5"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if a.Runtime().Pool == before.Pool {
		t.Fatal("改了 proxy 应新建连接池")
	}
	// 去掉
	if _, err := a.SetOverride("proxy", nil, "admin:alice"); err != nil || a.Runtime().Config.Proxy.MaxRedirects != 3 {
		t.Fatalf("重置后回到配置文件的值：%v %d", err, a.Runtime().Config.Proxy.MaxRedirects)
	}

	// 指标
	_, _ = a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice")
	rec := httptest.NewRecorder()
	a.Metrics.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `relay_config_overrides{key="cache",state="active"} 1`) {
		t.Fatalf("指标：\n%s", rec.Body.String())
	}
}

func TestSetOverrideRejectedChangesNothing(t *testing.T) {
	a, _ := newApp(t, "")
	_, _ = a.SetOverride("limits", ptr("max_inflight: 50"), "admin:alice")
	data, rt := dataJSON(t, a), a.Runtime()
	var oe *OverrideError
	for _, c := range []struct{ key, value string }{
		{"entrances", "- {hosts: [a.example.com], upstream: nope.example.com}"}, // 合起来通不过校验
		{"limits", "tracked_clients: 5"},                                        // 不能改的字段
		{"listen", "http: :80"},                                                 // 不能改的段
		{"cache", "tag_ttl: soon"},                                              // 写法不对
	} {
		if _, err := a.SetOverride(c.key, ptr(c.value), "admin:alice"); !errors.As(err, &oe) {
			t.Fatalf("%s 应被拒绝：%v", c.key, err)
		}
		if dataJSON(t, a) != data || a.Runtime() != rt {
			t.Fatalf("%s 被拒绝时运营数据库和生效的设置都不能变", c.key)
		}
	}
	// 去掉不存在的修改：什么都不做
	if _, err := a.SetOverride("cache", nil, "admin:alice"); err != nil || a.Runtime() != rt {
		t.Fatalf("去掉不存在的修改：%v", err)
	}
	// 新的提醒
	w, err := a.SetOverride("upstreams/cr.example.com", ptr("registry: registry.example.com"), "admin:alice")
	if err != nil || !strings.Contains(strings.Join(w, "\n"), "token_realm_hosts") {
		t.Fatalf("应返回这次修改带来的提醒：%v %v", w, err)
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
			t.Fatalf("%s：%v", c.key, err)
		}
	}

	// 配置文件改了限流（drift），并且给 quay.io 加了别名 cr.example.com，和后台新建的上游重名。
	// 配置文件是基准：这个上游暂停，引用它的入口也跟着暂停，不相关的照常生效。
	rewrite(t, a, path, "limits:\n  manifest_rate: 3\nupstreams:\n  quay.io:\n    aliases: [cr.example.com]\n")
	rt := a.Runtime()
	if st := stateOf(a, "limits"); st.State != OverrideDrift || rt.Config.Limits.ManifestRate != 7 {
		t.Fatalf("配置文件改过这个字段：仍以后台为准，标为 drift：%+v %v", st, rt.Config.Limits.ManifestRate)
	}
	if st := stateOf(a, "upstreams/cr.example.com"); st.State != OverrideSuspended || !strings.Contains(st.Error, "cr.example.com") {
		t.Fatalf("和配置文件冲突的修改暂停：%+v", st)
	}
	// 入口引用了暂停的上游，也跟着暂停；不相关的照常生效
	if st := stateOf(a, "entrances"); st.State != OverrideSuspended {
		t.Fatalf("依赖暂停的上游的入口也暂停：%+v", st)
	}
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideActive || !rt.Config.Upstreams["ghcr.io"].Disabled {
		t.Fatalf("不冲突的照常生效：%+v", st)
	}
	if rt.Config.Entrances[0].Hosts[0] != "*" {
		t.Fatalf("入口用配置文件的：%+v", rt.Config.Entrances)
	}
	if len(a.Store.Data().ConfigOverrides) != 4 {
		t.Fatal("暂停的修改仍然保存着")
	}

	// 暂停的修改不影响保存别的
	if _, err := a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(a, "entrances"); st.State != OverrideSuspended {
		t.Fatalf("保存别的修改时，暂停的保持暂停：%+v", st)
	}

	// 冲突解决了就恢复：先上游、再入口，一次重新加载都恢复
	rewrite(t, a, path, "limits:\n  manifest_rate: 3\n")
	for _, k := range []string{"upstreams/cr.example.com", "entrances"} {
		if st := stateOf(a, k); !st.Applied() {
			t.Fatalf("%s 应恢复：%+v", k, st)
		}
	}
	var actions []string
	for _, e := range a.Store.AuditLog(50) {
		actions = append(actions, e.Action+" "+e.Target)
	}
	for _, want := range []string{"config.override.suspended entrances", "config.override.resumed entrances"} {
		if !slices.Contains(actions, want) {
			t.Fatalf("审计里应有 %q：%v", want, actions)
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

	// 重启时配置文件和修改冲突：照常启动，冲突的暂停
	a = open("entrances:\n  - {hosts: [ghcr.example.com], upstream: ghcr.io}\n")
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideSuspended {
		t.Fatalf("重启时冲突的修改暂停：%+v", st)
	}
	if a.Runtime().Config.Limits.MaxInflight != 50 {
		t.Fatal("不冲突的照常生效")
	}
	// 在后台换掉引用 ghcr.io 的入口：冲突没了，暂停的修改跟着恢复
	if _, err := a.SetOverride("entrances", ptr("- {hosts: ['*']}"), "admin:alice"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(a, "upstreams/ghcr.io"); st.State != OverrideActive || !a.Runtime().Config.Upstreams["ghcr.io"].Disabled {
		t.Fatalf("冲突解决后暂停的修改恢复：%+v", st)
	}
	a.Close()

	// admin.config_edit: false：修改都不生效，不能保存，只能去掉
	a = open("admin: {config_edit: false}\n")
	defer a.Close()
	if st := stateOf(a, "limits"); st.State != OverrideIgnored || a.Runtime().Config.Limits.MaxInflight == 50 {
		t.Fatalf("config_edit 关掉时修改不生效：%+v", st)
	}
	if _, err := a.SetOverride("cache", ptr("tag_ttl: 1m"), "admin:alice"); !errors.Is(err, ErrConfigEditOff) {
		t.Fatalf("config_edit 关掉时不能保存：%v", err)
	}
	if _, err := a.SetOverride("limits", nil, "admin:alice"); err != nil || stateOf(a, "upstreams/ghcr.io").State != OverrideIgnored {
		t.Fatalf("可以去掉，其余仍是 ignored：%v %+v", err, a.Runtime().Overrides)
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
		t.Fatalf("只读存储：%v", err)
	}
}
