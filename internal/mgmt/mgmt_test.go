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

// newApp 用一个配置文件建 App，返回 App 和配置文件路径（改写后可以 ReloadConfig）。
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
		{"本机直连（kubectl port-forward）", plain, request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/", nil), true},
		{"内网直连（Prometheus、局域网）", plain, request("10.42.3.7:5000", "10.42.0.9:8080", "/metrics", nil), true},
		{"IPv6 本机", plain, request("[::1]:5000", "[::1]:8080", "/admin/", nil), true},
		{"公网直连", plain, request("203.0.113.9:5000", "mirror.example.com", "/admin/", nil), false},
		{"内网对端但带 X-Forwarded-For（经代理）", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", xff("127.0.0.1")), false},
		{"内网对端但带 Via", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", map[string]string{"Via": "1.1 squid"}), false},
		{"内网对端但带 X-Forwarded-Proto", plain, request("10.42.0.1:5000", "mirror.example.com", "/admin/", map[string]string{"X-Forwarded-Proto": "https"}), false},
		{"没有写 proxied_allow 时不允许经代理", plain, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), false},
		{"经代理：放行的客户端", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), true},
		{"经代理：客户端不在名单里", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("192.0.2.1")), false},
		{"经代理：伪造的内网地址也不行", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", xff("127.0.0.1")), false},
		{"经代理：Host 不是 public_url", proxied, request("10.42.0.1:5000", "mirror.example.com", "/admin/", xff("198.51.100.7")), false},
		{"经代理：对端不是受信任的代理", proxied, request("192.168.1.5:5000", "admin.example.com", "/admin/", xff("198.51.100.7")), false},
		{"经代理：只有 Forwarded 头，取不到客户端", proxied, request("10.42.0.1:5000", "admin.example.com", "/admin/", map[string]string{"Forwarded": "for=198.51.100.7"}), false},
	}
	for _, c := range cases {
		if got, reason := Allowed(c.a.Runtime(), c.r); got != c.want {
			t.Errorf("%s：期望 %v，实际 %v（%s）", c.name, c.want, got, reason)
		}
	}
	old := request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/", nil)
	old.Proto, old.ProtoMajor, old.ProtoMinor = "HTTP/1.0", 1, 0
	if ok, _ := Allowed(plain.Runtime(), old); ok {
		t.Fatal("HTTP/1.0 当作经过代理（nginx 默认的 proxy_pass），不放行")
	}
	closed, _ := newApp(t, "admin: {allow: []}")
	if ok, _ := Allowed(closed.Runtime(), request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/", nil)); ok {
		t.Fatal("admin.allow 为空时谁都进不来")
	}
}

// pull 模拟拉取入口：未知路径 404，并记下访问日志里的说明。
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
	a, path := newApp(t, "")
	h, err := New(a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	next := &pull{}
	s := h.Shared(next)

	if code, body := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/admin/login", nil)); code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("本机直连应能打开登录页：%d", code)
	}
	// 被拒绝时交给拉取入口，404 和任何未知路径一样，访问日志里写明原因
	code, body := serve(s, request("203.0.113.9:5000", "mirror.example.com", "/admin/login", nil))
	if !strings.Contains(next.note, "admin.allow") {
		t.Fatalf("访问日志应写明原因：%q", next.note)
	}
	_, plain404 := serve(next, request("203.0.113.9:5000", "mirror.example.com", "/no-such-path", nil))
	if code != 404 || body != plain404 {
		t.Fatalf("被拒绝的后台请求应和普通 404 一样：%d %q", code, body)
	}
	// 指标默认关：即使本机直连也是普通 404
	next.note = ""
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/metrics", nil)); code != 404 || next.note != "" {
		t.Fatalf("指标默认关：%d %q", code, next.note)
	}
	// 热重载打开指标和 pprof
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, []byte("metrics: {enabled: true}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if code, body := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/metrics", nil)); code != 200 || !strings.Contains(body, "relay_") {
		t.Fatalf("打开后应能抓指标：%d", code)
	}
	if code, _ := serve(s, request("203.0.113.9:5000", "mirror.example.com", "/metrics", nil)); code != 404 {
		t.Fatalf("公网仍然抓不到指标：%d", code)
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/debug/pprof/", nil)); code != 404 {
		t.Fatalf("没开 pprof 时是 404：%d", code)
	}
	if err := os.WriteFile(path, append(b, []byte("metrics: {enabled: true, pprof: true}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/debug/pprof/", nil)); code != 200 {
		t.Fatalf("开了 pprof 后应能打开：%d", code)
	}
	// 其他路径照常交给拉取入口
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:8080", "/v2/", nil)); code != 404 || next.note != "" {
		t.Fatal("非管理路径应交给拉取入口")
	}
}

func TestStandalone(t *testing.T) {
	a, _ := newApp(t, `listen: {admin: "127.0.0.1:9090"}`)
	h, err := New(a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := h.Standalone(slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, request("127.0.0.1:5000", "127.0.0.1:9090", "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("根路径应跳到后台：%d %s", rec.Code, rec.Header().Get("Location"))
	}
	if code, _ := serve(s, request("127.0.0.1:5000", "127.0.0.1:9090", "/admin/login", nil)); code != 200 {
		t.Fatalf("本机直连应能打开：%d", code)
	}
	for _, r := range []*http.Request{
		request("203.0.113.9:5000", "127.0.0.1:9090", "/admin/login", nil),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/admin/login", map[string]string{"X-Forwarded-For": "127.0.0.1"}),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/v2/", nil),
		request("127.0.0.1:5000", "127.0.0.1:9090", "/metrics", nil),
	} {
		if code, _ := serve(s, r); code != 404 {
			t.Errorf("%s %s 应是 404：%d", r.RemoteAddr, r.URL.Path, code)
		}
	}
}
