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
	res, err := config.Parse([]byte(`
log: {access: {output: "off"}}
admin:
  api_tokens:
    - {name: ops, role: admin, token: {env: T_ADMIN}}
    - {name: grafana, role: viewer, token: {env: T_VIEWER}}
`))
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
	a, err := app.New(res, nil, app.Options{Log: slog.New(slog.DiscardHandler), Store: st})
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
		t.Fatalf("没带令牌应返回 401：%d", code)
	}
	if code, _, _ := e.call(t, "GET", "/status", viewerTok, nil); code != 200 {
		t.Fatalf("viewer 可以读：%d", code)
	}
	if code, m, _ := e.call(t, "GET", "/status/report", viewerTok, nil); code != 200 || m["status"] == nil || m["upstreams"] == nil {
		t.Fatalf("服务状态报告：%d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/bans", viewerTok, map[string]string{"cidr": "198.51.100.7", "duration": "1h"}); code != 403 {
		t.Fatalf("viewer 不能写：%d", code)
	}
	// 用非回环地址访问（Host 不对）
	req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/status", nil)
	req.Host = "admin.evil.example"
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("没配置 public_url 时只接受回环地址：%d", resp.StatusCode)
	}
	// 认证失败太多次
	var last int
	for i := 0; i < 12; i++ {
		last, _, _ = e.call(t, "GET", "/status", "wrong-token", nil)
	}
	if last != 429 {
		t.Fatalf("认证失败太多次应返回 429：%d", last)
	}
}

func TestUsers(t *testing.T) {
	e := newEnv(t, nil)
	code, _, raw := e.call(t, "POST", "/users", adminTok, map[string]any{"name": "ci", "password": "correct horse battery", "manifest_rate": 10})
	if code != 201 {
		t.Fatalf("创建用户：%d %s", code, raw)
	}
	u := e.app.Runtime().Users["ci"]
	if u == nil || u.Source != "store" || u.ManifestRPS != 10 {
		t.Fatalf("用户应立即生效：%+v", u)
	}
	if ok, _ := e.app.Auth.Verify(u, "correct horse battery"); !ok {
		t.Fatal("口令应能校验")
	}
	if code, _, _ := e.call(t, "POST", "/users", adminTok, map[string]any{"name": "ci", "password": "correct horse battery"}); code != 409 {
		t.Fatal("重名应返回 409")
	}
	for _, b := range []map[string]any{
		{"name": "bad name", "password": "correct horse battery"},
		{"name": "x", "password": "short"},
		{"name": "x"},
		{"name": "x", "hash": "{SHA}abc"},
		{"name": "x", "password": "correct horse battery", "extra": 1},
	} {
		if code, _, _ := e.call(t, "POST", "/users", adminTok, b); code != 400 {
			t.Errorf("应拒绝 %v：%d", b, code)
		}
	}
	if code, _, _ := e.call(t, "PATCH", "/users/ci", adminTok, map[string]any{"disabled": true}); code != 200 || !e.app.Runtime().Users["ci"].Disabled {
		t.Fatal("停用应生效")
	}
	code, _, raw = e.call(t, "GET", "/users", viewerTok, nil)
	if code != 200 || strings.Contains(string(raw), "$2a$") || !strings.Contains(string(raw), `"ci"`) {
		t.Fatalf("列表不应含哈希：%s", raw)
	}
	if code, _, _ := e.call(t, "DELETE", "/users/ci", adminTok, nil); code != 204 || e.app.Runtime().Users["ci"] != nil {
		t.Fatal("删除应生效")
	}
	if code, _, _ := e.call(t, "DELETE", "/users/ci", adminTok, nil); code != 404 {
		t.Fatal("删除不存在的用户应返回 404")
	}
}

func TestRulesAndBans(t *testing.T) {
	e := newEnv(t, nil)
	ip := netip.MustParseAddr("198.51.100.7")
	if code, _, raw := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "198.51.100.7", "duration": "2h", "reason": "刷流量"}); code != 201 {
		t.Fatalf("封禁：%d %s", code, raw)
	}
	if ok, _ := e.app.Runtime().IPRules.Decide(ip, time.Now()); ok {
		t.Fatal("封禁应立即生效")
	}
	if code, _, _ := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "0.0.0.0/0", "duration": "1h"}); code != 400 {
		t.Fatal("不能封掉所有地址")
	}
	if code, _, _ := e.call(t, "DELETE", "/bans?cidr=198.51.100.7", adminTok, nil); code != 204 {
		t.Fatal("解除封禁")
	}
	if ok, _ := e.app.Runtime().IPRules.Decide(ip, time.Now()); !ok {
		t.Fatal("解除封禁应立即生效")
	}

	if code, _, raw := e.call(t, "POST", "/rules/image", adminTok, map[string]any{"pattern": "ghcr.io/evil/**", "kind": "deny", "status": 451, "message": "投诉屏蔽"}); code != 201 {
		t.Fatalf("加镜像规则：%d %s", code, raw)
	}
	_, m, _ := e.call(t, "POST", "/rules/image/test", viewerTok, map[string]string{"image": "ghcr.io/evil/miner:latest"})
	if m["allowed"] != false || m["status"].(float64) != 451 || m["image"] != "ghcr.io/evil/miner" {
		t.Fatalf("测试镜像：%v", m)
	}
	_, m, _ = e.call(t, "POST", "/rules/image/test", viewerTok, map[string]string{"image": "nginx:1.27"})
	if m["allowed"] != true || m["image"] != "docker.io/library/nginx" {
		t.Fatalf("没写上游时按 Docker Hub：%v", m)
	}
	if code, _, _ := e.call(t, "POST", "/rules/image", adminTok, map[string]any{"pattern": "example.org/x", "kind": "deny"}); code != 400 {
		t.Fatal("没配置的上游应拒绝")
	}
	if code, _, _ := e.call(t, "POST", "/rules/ip", adminTok, map[string]any{"cidr": "203.0.113.0/24", "kind": "deny"}); code != 201 {
		t.Fatal("加 IP 规则")
	}
	_, m, _ = e.call(t, "GET", "/rules", viewerTok, nil)
	if len(m["images"].([]any)) != 1 || len(m["ip"].([]any)) != 1 {
		t.Fatalf("规则列表：%v", m)
	}
	// 审计
	code, _, raw := e.call(t, "GET", "/audit", viewerTok, nil)
	if code != 200 || !strings.Contains(string(raw), `"ban.add"`) || !strings.Contains(string(raw), `"token:ops"`) {
		t.Fatalf("审计：%s", raw)
	}
}

func TestTokensAndRedirectHosts(t *testing.T) {
	e := newEnv(t, nil)
	_, m, _ := e.call(t, "POST", "/tokens", adminTok, map[string]string{"name": "ci", "role": "viewer"})
	tok, _ := m["token"].(string)
	if !strings.HasPrefix(tok, "rmr_") {
		t.Fatalf("应返回新令牌：%v", m)
	}
	if code, _, _ := e.call(t, "GET", "/status", tok, nil); code != 200 {
		t.Fatal("新令牌应能用")
	}
	if code, _, _ := e.call(t, "POST", "/tokens", adminTok, map[string]string{"name": "ops", "role": "viewer"}); code != 409 {
		t.Fatal("和配置里的令牌重名应拒绝")
	}
	if code, _, _ := e.call(t, "DELETE", "/tokens/ci", adminTok, nil); code != 204 {
		t.Fatal("删除令牌")
	}
	if code, _, _ := e.call(t, "GET", "/status", tok, nil); code != 401 {
		t.Fatal("删掉的令牌不能再用")
	}

	if code, _, raw := e.call(t, "POST", "/upstreams/quay.io/redirect-hosts", adminTok, map[string]string{"host": "cdn99.quay.io"}); code != 201 {
		t.Fatalf("加重定向主机：%d %s", code, raw)
	}
	if code, _, _ := e.call(t, "POST", "/upstreams/quay.io/redirect-hosts", adminTok, map[string]string{"host": "*.evil.com"}); code != 400 {
		t.Fatal("不接受通配")
	}
	_, _, raw := e.call(t, "GET", "/upstreams", viewerTok, nil)
	if !strings.Contains(string(raw), `cdn99\\.quay\\.io`) {
		t.Fatalf("白名单应带上新主机（转义成正则）：%s", raw)
	}
}

func TestReadOnlyStoreAndMisc(t *testing.T) {
	e := newEnv(t, store.NewReadOnly())
	if code, m, _ := e.call(t, "POST", "/bans", adminTok, map[string]string{"cidr": "198.51.100.7", "duration": "1h"}); code != 409 || !strings.Contains(m["error"].(string), "只读") {
		t.Fatalf("只读存储应返回 409：%d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/reload", adminTok, map[string]string{}); code != 422 {
		t.Fatalf("没有配置文件时重载应返回 422：%d", code)
	}
	for _, p := range []string{"/stats/series?minutes=10", "/stats/summary?hours=24", "/requests?limit=5", "/config", "/tokens", "/upstreams"} {
		if code, _, raw := e.call(t, "GET", p, viewerTok, nil); code != 200 {
			t.Errorf("%s：%d %s", p, code, raw)
		}
	}
	_, m, _ := e.call(t, "GET", "/config", viewerTok, nil)
	if y := m["yaml"].(string); strings.Contains(y, adminTok) || !strings.Contains(y, "T_ADMIN") {
		t.Fatal("配置里不能有明文令牌")
	}
	// 不是 JSON 的请求体
	req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/bans", strings.NewReader("cidr=1"))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON 应返回 415：%d", resp.StatusCode)
	}
}

func TestAdminsAndSite(t *testing.T) {
	e := newEnv(t, nil)
	if code, _, raw := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "alice", "password": "alice-password-1", "role": "admin"}); code != 201 {
		t.Fatalf("创建管理员：%d %s", code, raw)
	}
	if code, _, _ := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "alice", "password": "alice-password-1", "role": "admin"}); code != 409 {
		t.Fatal("重名应返回 409")
	}
	if code, _, _ := e.call(t, "POST", "/admins", adminTok, map[string]string{"name": "bob", "password": "short", "role": "admin"}); code != 400 {
		t.Fatal("口令太短应返回 400")
	}
	if code, m, _ := e.call(t, "PATCH", "/admins/alice", adminTok, map[string]any{"role": "viewer"}); code != 409 || !strings.Contains(m["error"].(string), "至少要留一个") {
		t.Fatalf("不能去掉最后一个 admin：%d %v", code, m)
	}
	_, _, raw := e.call(t, "GET", "/admins", viewerTok, nil)
	if strings.Contains(string(raw), "$2a$") || !strings.Contains(string(raw), `"alice"`) {
		t.Fatalf("列表不含哈希：%s", raw)
	}
	if code, _, _ := e.call(t, "DELETE", "/admins/alice", adminTok, nil); code != 409 {
		t.Fatal("不能删掉最后一个 admin")
	}

	code, m, raw := e.call(t, "PUT", "/site", adminTok, map[string]any{"name": "Test Mirrors", "operator": "Test Org", "links": map[string]string{"status": "https://status.example.org"}})
	if code != 200 || m["source"] != "store" || e.app.Runtime().Site.Name != "Test Mirrors" {
		t.Fatalf("站点信息：%d %s", code, raw)
	}
	if code, _, _ := e.call(t, "PUT", "/site", adminTok, map[string]any{"name": "x", "links": map[string]string{"status": "javascript:alert(1)"}}); code != 400 {
		t.Fatal("链接只接受 http(s)")
	}
	if code, m, _ := e.call(t, "DELETE", "/site", adminTok, nil); code != 200 || m["source"] != "config" {
		t.Fatalf("改回配置文件：%d %v", code, m)
	}
	if code, _, _ := e.call(t, "POST", "/upstreams/nope.example/check", adminTok, nil); code != 404 {
		t.Fatalf("探测不存在的上游：%d", code)
	}
}
