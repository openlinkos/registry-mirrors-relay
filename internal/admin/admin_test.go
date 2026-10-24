package admin

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/api"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

const (
	alicePW  = "alice-password-123"
	victorPW = "victor-password-123"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	app    *app.App
	h      *Handler
	mu     sync.Mutex
	clock  time.Time
	origin string
	host   string // 不为空时作为请求的 Host
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

type envOpts struct {
	yaml  string
	store store.Store
	noAdm bool
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	raw := []byte("log: {access: {output: \"off\"}}\nproxy: {health_interval: 0s}\n" + o.yaml)
	res, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	st := o.store
	if st == nil {
		bs, err := store.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		st = bs
		if !o.noAdm {
			hash := func(p string) string {
				h, _ := bcrypt.GenerateFromPassword([]byte(p), bcrypt.MinCost)
				return string(h)
			}
			if err := st.Update(func(d *store.Data) error {
				d.Admins = []store.Admin{
					{Name: "alice", Hash: hash(alicePW), Role: "admin", Created: time.Now()},
					{Name: "victor", Hash: hash(victorPW), Role: "viewer", Created: time.Now()},
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a, err := app.New(res, raw, app.Options{Log: slog.New(slog.DiscardHandler), Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e := &env{t: t, app: a, clock: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	op := ops.New(a)
	h, err := New(a, op, Options{Log: slog.New(slog.DiscardHandler), Now: e.now, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", api.New(a, op, api.Options{Session: h.Session}))
	mux.Handle("/admin/", h)
	mux.Handle("/admin", h)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.origin = e.srv.URL
	return e
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type resp struct {
	*http.Response
	body string
}

func (r resp) cookie(name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// do 发一个请求。cookies 是 名字=值 的列表；POST 默认带上正确的 Origin 和表单类型。
func (e *env) do(method, path string, form url.Values, cookies []string, hdr map[string]string) resp {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.host != "" {
		req.Host = e.host
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.origin)
	}
	for _, c := range cookies {
		req.Header.Add("Cookie", c)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	r, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r, string(b)}
}

func (e *env) get(path string, cookies ...string) resp { return e.do("GET", path, nil, cookies, nil) }

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

type testSession struct {
	cookie string // 名字=值
	csrf   string
}

// login 登录并取出会话 Cookie 和 CSRF 令牌。
func (e *env) login(user, pw string) testSession {
	e.t.Helper()
	r := e.do("POST", "/admin/login", url.Values{"username": {user}, "password": {pw}, "next": {"/admin/"}}, nil, nil)
	if r.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("登录 %s：%d %s", user, r.StatusCode, r.body)
	}
	c := r.Cookies()[0]
	s := testSession{cookie: c.Name + "=" + c.Value}
	page := e.get("/admin/", s.cookie)
	m := csrfRe.FindStringSubmatch(page.body)
	if m == nil {
		e.t.Fatalf("页面里没有 CSRF 令牌：%d %s", page.StatusCode, page.body)
	}
	s.csrf = m[1]
	return s
}

func (e *env) post(s testSession, path string, form url.Values) resp {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("_csrf", s.csrf)
	return e.do("POST", path, form, []string{s.cookie}, nil)
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.get("/admin/login")
	if r.StatusCode != 200 {
		t.Fatalf("登录页：%d", r.StatusCode)
	}
	want := map[string]string{
		"Content-Security-Policy":      "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; require-trusted-types-for 'script'; trusted-types 'none'",
		"Cache-Control":                "no-store, private",
		"Vary":                         "Cookie",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "same-origin",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), bluetooth=(), hid=(), midi=()",
		"X-Robots-Tag":                 "noindex, nofollow",
		"Content-Type":                 "text/html; charset=utf-8",
	}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s = %q，应为 %q", k, got, v)
		}
	}
	if r.Header.Get("Strict-Transport-Security") != "" {
		t.Error("没有 https 的 public_url 时不应发 HSTS")
	}
	// 根路径和 /admin
	if r := e.get("/admin"); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/" {
		t.Errorf("/admin 应跳到 /admin/：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.get("/admin/"); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login?next=%2Fadmin%2F" {
		t.Errorf("没登录应跳到登录页：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.do("PUT", "/admin/", nil, nil, nil); r.StatusCode != 405 || r.Header.Get("Allow") != "GET, POST" {
		t.Errorf("只允许 GET 和 POST：%d %s", r.StatusCode, r.Header.Get("Allow"))
	}
	if r := e.get("/admin/nope"); r.StatusCode != 404 {
		t.Errorf("不存在的页面：%d", r.StatusCode)
	}
}

func TestHostCheck(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.host = "admin.evil.example"
	if r := e.get("/admin/login"); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("没配置 public_url 时只接受回环地址：%d", r.StatusCode)
	}
	if r := e.get(api.Prefix + "/status"); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("API 也一样：%d", r.StatusCode)
	}
	e.host = "localhost"
	if r := e.get("/admin/login"); r.StatusCode != 200 {
		t.Fatalf("localhost 可以：%d", r.StatusCode)
	}
	// 局域网里用 IP 直接打开：没有 DNS 重绑定的问题
	for _, h := range []string{"192.0.2.10:8080", "[2001:db8::10]:8080"} {
		e.host = h
		if r := e.get("/admin/login"); r.StatusCode != 200 {
			t.Fatalf("IP %s 可以：%d", h, r.StatusCode)
		}
	}
	// 用 IP 打开时，Origin 就是 http://<IP>，可以登录
	e.host, e.origin = "192.0.2.10:8080", "http://192.0.2.10:8080"
	if r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("用 IP 打开后台应能登录：%d", r.StatusCode)
	}
}

func TestLoginAndCookieFlags(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {"wrong-password-1"}}, nil, nil)
	if r.StatusCode != 401 || !strings.Contains(r.body, tr("zh", "auth.bad")) || len(r.Cookies()) != 0 {
		t.Fatalf("口令错误：%d", r.StatusCode)
	}
	r = e.do("POST", "/admin/login", url.Values{"username": {"nobody"}, "password": {"whatever-123456"}}, nil, nil)
	if r.StatusCode != 401 {
		t.Fatalf("用户不存在也是 401：%d", r.StatusCode)
	}
	r = e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}, "next": {"/admin/rules?x=1"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/rules?x=1" {
		t.Fatalf("登录后跳到 next：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	c := r.cookie(plainCookie)
	if c == nil || !c.HttpOnly || c.Secure || c.Path != "/admin" || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 0 {
		t.Fatalf("回环地址上的会话 Cookie：%+v", c)
	}
	if !strings.Contains(r.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("Set-Cookie：%s", r.Header.Get("Set-Cookie"))
	}
	page := e.get("/admin/", plainCookie+"="+c.Value)
	if page.StatusCode != 200 || !strings.Contains(page.body, "alice") {
		t.Fatalf("登录后能看概览：%d", page.StatusCode)
	}
	// 开放跳转
	r = e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}, "next": {"//evil.example/"}}, nil, nil)
	if r.Header.Get("Location") != "/admin/" {
		t.Fatalf("next 不能指向别的站点：%s", r.Header.Get("Location"))
	}
	// 退出
	s := testSession{cookie: plainCookie + "=" + c.Value}
	s.csrf = csrfRe.FindStringSubmatch(page.body)[1]
	if r := e.post(s, "/admin/logout", nil); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" {
		t.Fatalf("退出：%d", r.StatusCode)
	}
	if r := e.get("/admin/", s.cookie); r.StatusCode != 303 {
		t.Fatalf("退出后会话失效：%d", r.StatusCode)
	}
	log := e.app.Store.AuditLog(100)
	var actions []string
	for _, a := range log {
		actions = append(actions, a.Action)
	}
	if strings.Count(strings.Join(actions, ","), "login.failed") != 1 || !slices.Contains(actions, "login") || !slices.Contains(actions, "logout") {
		t.Fatalf("审计：失败只记第一次，登录、退出都要记：%v", actions)
	}
}

func TestSecureCookieBehindHTTPS(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "admin: {public_url: \"https://admin.example.test\"}\n"})
	e.host, e.origin = "admin.example.test", "https://admin.example.test"
	r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil)
	c := r.cookie(secureCookie)
	if r.StatusCode != 303 || c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("https 时用 __Host- Cookie：%d %+v", r.StatusCode, r.Cookies())
	}
	if r.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatal("https 的 public_url 要发 HSTS")
	}
	if r := e.get("/admin/", secureCookie+"="+c.Value); r.StatusCode != 200 {
		t.Fatalf("带 __Host- Cookie 能访问：%d", r.StatusCode)
	}
	// 同一个会话 ID 放在普通 Cookie 里不认（兄弟子域名可以种普通 Cookie）
	if r := e.get("/admin/", plainCookie+"="+c.Value); r.StatusCode != 303 {
		t.Fatalf("https 下不认普通 Cookie：%d", r.StatusCode)
	}
	// http 的来源不行
	e.origin = "http://admin.example.test"
	if r := e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {alicePW}}, nil, nil); r.StatusCode != 403 {
		t.Fatalf("来源要和 public_url 完全一致：%d", r.StatusCode)
	}
}

func TestPostChecks(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	form := func() url.Values {
		return url.Values{"_csrf": {s.csrf}, "cidr": {"198.51.100.7"}, "duration": {"1h"}}
	}
	// 改掉第一个字符；原来就是 x 时改成 y，否则改出来的还是同一个令牌
	wrong := "x" + s.csrf[1:]
	if wrong == s.csrf {
		wrong = "y" + s.csrf[1:]
	}
	cases := []struct {
		name string
		hdr  map[string]string
		form url.Values
		want int
	}{
		{"没有 Origin", map[string]string{"Origin": ""}, form(), 403},
		{"外来 Origin", map[string]string{"Origin": "http://evil.example"}, form(), 403},
		{"跨站", map[string]string{"Sec-Fetch-Site": "cross-site"}, form(), 403},
		{"没有 CSRF", nil, url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}, 403},
		{"错误的 CSRF", nil, url.Values{"_csrf": {wrong}, "cidr": {"198.51.100.7"}, "duration": {"1h"}}, 403},
		{"不是表单", map[string]string{"Content-Type": "application/json"}, form(), 415},
	}
	for _, c := range cases {
		if r := e.do("POST", "/admin/bans/add", c.form, []string{s.cookie}, c.hdr); r.StatusCode != c.want {
			t.Errorf("%s：%d，应为 %d", c.name, r.StatusCode, c.want)
		}
	}
	if len(e.app.Runtime().Data.Bans) != 0 {
		t.Fatal("被拒绝的请求不能生效")
	}
	r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}, "reason": {"测试"}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/rules?ok=ban.added&v=198.51.100.7%2F32" {
		t.Fatalf("封禁：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if page := e.get(r.Header.Get("Location"), s.cookie); !strings.Contains(page.body, "已封禁 198.51.100.7/32。") {
		t.Fatal("跳转回来后应显示成功提示")
	}
	// 只读角色
	v := e.login("victor", victorPW)
	if r := e.post(v, "/admin/bans/delete", url.Values{"cidr": {"198.51.100.7"}}); r.StatusCode != 403 {
		t.Fatalf("只读角色不能修改：%d", r.StatusCode)
	}
	// 没登录的 POST 跳到登录页
	if r := e.do("POST", "/admin/bans/add", form(), nil, nil); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" {
		t.Fatalf("没登录：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// 表单的错误：带着说明重新渲染
	r = e.post(s, "/admin/bans/add", url.Values{"cidr": {"0.0.0.0/0"}, "duration": {"1h"}})
	if r.StatusCode != 400 || !strings.Contains(r.body, "不能整段放开或封掉所有地址") || !strings.Contains(r.body, `value="0.0.0.0/0"`) {
		t.Fatalf("错误应重新渲染并保留填过的值：%d", r.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, envOpts{})
	try := func(pw string) resp {
		return e.do("POST", "/admin/login", url.Values{"username": {"alice"}, "password": {pw}}, nil, nil)
	}
	for i := 1; i <= 10; i++ {
		if r := try("wrong-password-1"); r.StatusCode != 401 {
			t.Fatalf("第 %d 次失败应返回 401：%d", i, r.StatusCode)
		}
	}
	r := try(alicePW)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" || len(r.Cookies()) != 0 {
		t.Fatalf("10 次失败后口令正确也要拒绝：%d %s", r.StatusCode, r.Header.Get("Retry-After"))
	}
	try(alicePW)
	e.advance(10*time.Minute + time.Second)
	if r := try(alicePW); r.StatusCode != 303 {
		t.Fatalf("窗口过去后可以登录：%d", r.StatusCode)
	}
	var failed, blocked int
	for _, a := range e.app.Store.AuditLog(100) {
		switch a.Action {
		case "login.failed":
			failed++
		case "login.blocked":
			blocked++
		}
	}
	if failed != 1 || blocked != 1 {
		t.Fatalf("每个窗口只记一次失败和一次封锁：failed=%d blocked=%d", failed, blocked)
	}
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	e.advance(31 * time.Minute)
	if r := e.get("/admin/rules", s.cookie); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login?next=%2Fadmin%2Frules" {
		t.Fatalf("空闲 30 分钟后要重新登录：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	s = e.login("alice", alicePW)
	start := e.now()
	for e.now().Sub(start) < 12*time.Hour-29*time.Minute {
		e.advance(29 * time.Minute)
		if r := e.get("/admin/", s.cookie); r.StatusCode != 200 {
			t.Fatalf("一直有操作时 12 小时内有效（%s）：%d", e.now().Sub(start), r.StatusCode)
		}
	}
	e.advance(29 * time.Minute)
	if r := e.get("/admin/", s.cookie); r.StatusCode != 303 {
		t.Fatalf("登录满 12 小时后失效（%s）：%d", e.now().Sub(start), r.StatusCode)
	}
}

func TestFreshForSensitiveActions(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	r := e.post(s, "/admin/tokens/create", url.Values{"name": {"ci"}, "role": {"viewer"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, "rmr_") {
		t.Fatalf("刚登录时可以直接创建令牌：%d", r.StatusCode)
	}
	e.advance(11 * time.Minute)
	r = e.post(s, "/admin/tokens/create", url.Values{"name": {"ci2"}, "role": {"viewer"}})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/reauth?next=%2Fadmin%2Fadmins" {
		t.Fatalf("超过 10 分钟要重新输入口令：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.get("/admin/reauth?next=%2Fadmin%2Fadmins", s.cookie); r.StatusCode != 200 {
		t.Fatalf("确认身份页：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/reauth", url.Values{"password": {"wrong-password-1"}, "next": {"/admin/admins"}}); r.StatusCode != 401 {
		t.Fatalf("口令错误：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/reauth", url.Values{"password": {alicePW}, "next": {"/admin/admins"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/admins" {
		t.Fatalf("确认身份后回到原页面：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.post(s, "/admin/tokens/create", url.Values{"name": {"ci2"}, "role": {"viewer"}}); r.StatusCode != 200 {
		t.Fatalf("确认身份后可以创建：%d", r.StatusCode)
	}
}

func TestWriteRoutesPinned(t *testing.T) {
	e := newEnv(t, envOpts{})
	want := []string{
		"/admin/account/password", "/admin/admins/create", "/admin/admins/delete", "/admin/admins/update",
		"/admin/bans/add", "/admin/bans/delete", "/admin/config/entrances/save", "/admin/config/reset",
		"/admin/config/save", "/admin/config/upstream/save", "/admin/login", "/admin/logout", "/admin/prefs", "/admin/reauth",
		"/admin/reload", "/admin/rules/image/add", "/admin/rules/image/delete", "/admin/rules/ip/add",
		"/admin/rules/ip/delete", "/admin/sessions/revoke", "/admin/settings/reset", "/admin/settings/site",
		"/admin/tokens/create", "/admin/tokens/delete", "/admin/upstreams/check", "/admin/upstreams/hosts/add",
		"/admin/upstreams/hosts/delete", "/admin/users/create", "/admin/users/delete", "/admin/users/update",
	}
	if got := e.h.WriteRoutes(); !slices.Equal(got, want) {
		t.Fatalf("后台的写操作变了：\n%v", got)
	}
	// 敏感操作都要求最近输入过口令
	for _, r := range e.h.routes {
		sensitive := strings.HasPrefix(r.path, "/admin/admins/") || strings.HasPrefix(r.path, "/admin/tokens/") ||
			strings.HasPrefix(r.path, "/admin/config/") || r.path == "/admin/sessions/revoke" || r.path == "/admin/account/password"
		if r.method == http.MethodPost && sensitive && r.auth != authFresh {
			t.Errorf("%s 应要求重新输入口令", r.path)
		}
	}
}

// fillStats 造一些请求记录，让各个页面都有内容。
func fillStats(a *app.App) {
	for i := range 6 {
		a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "198.51.100.9", Method: "GET", Host: "mirror.test",
			URI: "/v2/library/nginx/blobs/sha256:abc", Status: 200, Size: 1 << 20, RespContentLength: "1048576",
			Duration: time.Duration(i+1) * time.Millisecond, Upstream: "docker.io", Image: "docker.io/library/nginx",
			Kind: "blob", Ref: "sha256:abc", UserID: "bob"})
	}
	a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "203.0.113.5", Method: "GET", Host: "mirror.test",
		URI: "/v2/library/redis/manifests/7", Status: 429, Limited: true, Upstream: "docker.io", Image: "docker.io/library/redis", Kind: "manifest"})
	a.Log(&accesslog.Entry{Time: time.Now(), ClientIP: "203.0.113.6", Method: "GET", Host: "mirror.test",
		URI: "/v2/x/manifests/1", Status: 502, Error: "<script>alert(1)</script>", Upstream: "ghcr.io", Image: "ghcr.io/x", Kind: "manifest"})
}

var (
	inlineStyle  = regexp.MustCompile(`(?i)\sstyle=`)
	inlineScript = regexp.MustCompile(`(?i)<script(\s[^>]*)?>`)
	eventAttr    = regexp.MustCompile(`(?i)\son[a-z]+=`)
	iconUse      = regexp.MustCompile(`<use href="#i-([a-z0-9-]+)"/>`)
)

// checkPage 是每个页面都要满足的：没有内联样式、内联脚本、事件属性；用到的图标都有 symbol；没有漏翻的文案。
func checkPage(t *testing.T, name, body string) {
	t.Helper()
	if inlineStyle.MatchString(body) {
		t.Errorf("%s 有内联样式", name)
	}
	for _, m := range inlineScript.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(m[1], "src=") {
			t.Errorf("%s 有内联脚本", name)
		}
	}
	if eventAttr.MatchString(body) {
		t.Errorf("%s 有事件属性", name)
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Errorf("%s 没有转义", name)
	}
	for _, m := range iconUse.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `<symbol id="i-`+m[1]+`"`) {
			t.Errorf("%s 用了图标 %s 但没有 symbol", name, m[1])
		}
	}
	if strings.Contains(body, spriteMark) {
		t.Errorf("%s 的图标占位没有替换", name)
	}
	for k := range messagesZH {
		if strings.Contains(body, ">"+k+"<") || strings.Contains(body, `"`+k+`"`) {
			t.Errorf("%s 里有没翻译的键 %s", name, k)
		}
	}
}

func TestPagesRender(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "entrances:\n  - {name: mirror, hosts: [mirror.test]}\n  - {hosts: [ghcr.test], upstream: ghcr.io, auth: basic}\n"})
	admin := e.login("alice", alicePW)
	if r := e.post(admin, "/admin/users/create", url.Values{"name": {"bob"}, "password": {"bob-password-123"}, "manifest_rate": {"5"}}); r.StatusCode != 303 {
		t.Fatalf("建账号：%d %s", r.StatusCode, r.body)
	}
	e.post(admin, "/admin/rules/image/add", url.Values{"pattern": {"ghcr.io/evil/**"}, "kind": {"deny"}, "status": {"451"}})
	e.app.Stats.UnlistedRedirect("quay.io", "cdn99.quay.io")
	fillStats(e.app)
	viewer := e.login("victor", victorPW)
	pages := []string{
		"/admin/", "/admin/upstreams", "/admin/usage", "/admin/usage?host=mirror.test&scheme=http", "/admin/users",
		"/admin/users/edit?name=bob", "/admin/rules", "/admin/rules?image=ghcr.io/evil/miner:1", "/admin/rules?image=nginx",
		"/admin/limits", "/admin/requests", "/admin/requests?status=5&q=x", "/admin/requests?page=3", "/admin/abuse",
		"/admin/abuse?hours=168", "/admin/audit", "/admin/settings", "/admin/admins", "/admin/admins/edit?name=victor",
		"/admin/system", "/admin/account", "/admin/reauth", "/admin/config", "/admin/config/entrances",
		"/admin/config/upstream?name=ghcr.io", "/admin/config/upstream", "/admin/nope",
	}
	for _, lang := range []string{"zh", "en"} {
		for _, who := range []testSession{admin, viewer} {
			for _, p := range pages {
				r := e.get(p, who.cookie, langCookie+"="+lang)
				want := 200
				if p == "/admin/nope" {
					want = 404
				}
				if r.StatusCode != want {
					t.Fatalf("%s（%s）：%d %s", p, lang, r.StatusCode, r.body)
				}
				checkPage(t, p+" "+lang, r.body)
				if lang == "en" && !strings.Contains(r.body, `<html lang="en"`) {
					t.Errorf("%s 应是英文", p)
				}
			}
		}
	}
	for _, p := range []string{"/admin/login", "/admin/login?next=/admin/rules"} {
		r := e.get(p)
		checkPage(t, p, r.body)
	}
	// 只读角色看不到修改的表单
	if r := e.get("/admin/users", viewer.cookie); strings.Contains(r.body, `action="/admin/users/create"`) {
		t.Error("只读角色不应看到新建表单")
	}
	if r := e.get("/admin/users", admin.cookie); !strings.Contains(r.body, `action="/admin/users/create"`) {
		t.Error("管理员应看到新建表单")
	}
	// 概览的实时数字
	r := e.get("/admin/live", admin.cookie)
	var live map[string]string
	if r.StatusCode != 200 || json.Unmarshal([]byte(r.body), &live) != nil || live["rpm"] == "" {
		t.Fatalf("/admin/live：%d %s", r.StatusCode, r.body)
	}
	if r := e.do("GET", "/admin/live", nil, []string{admin.cookie}, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("/admin/live 不接受跨站：%d", r.StatusCode)
	}
}

func TestMessagesComplete(t *testing.T) {
	for k := range messagesZH {
		if _, ok := messagesEN[k]; !ok {
			t.Errorf("英文缺 %s", k)
		}
	}
	for k := range messagesEN {
		if _, ok := messagesZH[k]; !ok {
			t.Errorf("中文缺 %s", k)
		}
	}
	// 模板和代码里用到的键都要有
	used := regexp.MustCompile(`\.T "([a-zA-Z0-9_.]+)"|tr\((?:c\.)?lang, "([a-zA-Z0-9_.]+)"|fail\(http\.\w+, "([a-zA-Z0-9_.]+)"|message: "([a-zA-Z0-9_.]+)"`)
	check := func(name string, b []byte) {
		for _, m := range used.FindAllStringSubmatch(string(b), -1) {
			k := m[1] + m[2] + m[3] + m[4]
			if strings.HasSuffix(k, ".") {
				continue // 拼出来的键，下面单独检查
			}
			if _, ok := messagesZH[k]; !ok {
				t.Errorf("%s 用了没有的键 %s", name, k)
			}
		}
	}
	_ = fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			b, _ := templateFS.ReadFile(p)
			check(p, b)
		}
		return nil
	})
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		check(f, b)
	}
	// 拼出来的键
	for _, k := range []string{"theme.auto", "theme.light", "theme.dark", "role.admin", "role.viewer", "abuse.hours1", "abuse.hours24", "abuse.hours168"} {
		if _, ok := messagesZH[k]; !ok {
			t.Errorf("缺 %s", k)
		}
	}
	for _, k := range ops.SiteLinkKeys {
		if _, ok := messagesZH["settings.link."+k]; !ok {
			t.Errorf("缺 settings.link.%s", k)
		}
	}
	for _, k := range []string{"builtin", "config", "console"} {
		if _, ok := messagesZH["config.source."+k]; !ok {
			t.Errorf("缺 config.source.%s", k)
		}
	}
	for _, key := range append(append([]string{}, config.OverrideSections...), "upstreams/x.example.com") {
		sec, _, _ := strings.Cut(key, "/")
		if sec == key {
			for _, k := range []string{"cfg." + key, "cfg." + key + "Help"} {
				if _, ok := messagesZH[k]; !ok {
					t.Errorf("缺 %s", k)
				}
			}
		}
		for _, f := range config.OverrideFields(key) {
			if _, ok := messagesZH["cfg."+sec+"."+f]; !ok {
				t.Errorf("缺 cfg.%s.%s", sec, f)
			}
		}
	}
	for _, n := range notices {
		if _, ok := messagesZH["notice."+n]; !ok {
			t.Errorf("缺 notice.%s", n)
		}
	}
	for _, g := range navTable {
		for _, it := range g.items {
			if _, ok := messagesZH["nav."+it[0]]; !ok {
				t.Errorf("缺 nav.%s", it[0])
			}
		}
	}
}

func TestAssets(t *testing.T) {
	e := newEnv(t, envOpts{})
	page := e.get("/admin/login").body
	for _, name := range []string{"ui.css", "admin.css", "admin.js", "favicon.svg", "logo.svg"} {
		u := e.h.assets.url(name)
		if !strings.Contains(page, u) && name != "logo.svg" {
			t.Errorf("页面里应引用 %s", u)
		}
		r := e.get(u)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
			t.Fatalf("%s：%d %s", u, r.StatusCode, r.Header.Get("Cache-Control"))
		}
		wantCSP := CSP
		if strings.HasSuffix(name, ".svg") {
			wantCSP = SVGCSP
		}
		if r.Header.Get("Content-Security-Policy") != wantCSP {
			t.Errorf("%s 的 CSP：%s", name, r.Header.Get("Content-Security-Policy"))
		}
	}
	css := e.get(e.h.assets.url("ui.css")).body
	m := regexp.MustCompile(`url\((fonts/Geist-Variable\.[0-9a-f]{10}\.woff2)\)`).FindStringSubmatch(css)
	if m == nil || strings.Contains(css, "url(fonts/Geist-Variable.woff2)") {
		t.Fatal("样式表里的字体地址应改成带哈希的")
	}
	if r := e.get("/admin/assets/" + m[1]); r.StatusCode != 200 || r.Header.Get("Content-Type") != "font/woff2" {
		t.Fatalf("字体：%d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := e.get("/admin/assets/ui.css"); r.StatusCode != 404 {
		t.Fatalf("不带哈希的名字不提供：%d", r.StatusCode)
	}
	if r := e.do("POST", e.h.assets.url("admin.css"), url.Values{}, nil, nil); r.StatusCode != 405 {
		t.Fatalf("静态文件只能 GET：%d", r.StatusCode)
	}
}

func TestPrefs(t *testing.T) {
	e := newEnv(t, envOpts{})
	r := e.do("POST", "/admin/prefs", url.Values{"lang": {"en"}, "theme": {"dark"}, "next": {"/admin/login"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/login" || r.cookie(langCookie) == nil || r.cookie(themeCookie) == nil {
		t.Fatalf("偏好：%d %v", r.StatusCode, r.Cookies())
	}
	page := e.get("/admin/login", langCookie+"=en", themeCookie+"=dark")
	if !strings.Contains(page.body, `<html lang="en" data-theme="dark">`) || !strings.Contains(page.body, "Sign in") {
		t.Fatal("应是英文、深色")
	}
	if r := e.do("POST", "/admin/prefs", url.Values{"lang": {"en"}}, nil, map[string]string{"Origin": "http://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("偏好也要检查来源：%d", r.StatusCode)
	}
	if got := negotiateLang("", "en-US,en;q=0.9,zh;q=0.8"); got != "en" {
		t.Fatalf("Accept-Language：%s", got)
	}
	if got := negotiateLang("", "fr, zh-CN;q=0.5"); got != "zh" {
		t.Fatalf("Accept-Language：%s", got)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/admin/rules?x=1":        "/admin/rules?x=1",
		"/admin":                  "/admin",
		"":                        "/admin/",
		"https://evil.example/":   "/admin/",
		"//evil.example/admin/":   "/admin/",
		"/admin//evil.example":    "/admin/",
		"/admin/\\evil":           "/admin/",
		"/adminx":                 "/admin/",
		"/admin/api/v1/status":    "/admin/",
		"/admin/assets/x.css":     "/admin/",
		"/admin/ok\r\nSet-Cookie": "/admin/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestCRUDAndOwnPassword(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	// 不填口令时生成一个，只显示一次
	r := e.post(s, "/admin/users/create", url.Values{"name": {"ci"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, `id="secret-value"`) {
		t.Fatalf("生成口令：%d", r.StatusCode)
	}
	pw := regexp.MustCompile(`id="secret-value">([^<]+)<`).FindStringSubmatch(r.body)[1]
	if ok, _ := e.app.Auth.Verify(e.app.Runtime().Users["ci"], pw); !ok {
		t.Fatal("生成的口令应能用")
	}
	if r := e.post(s, "/admin/users/create", url.Values{"name": {"ci"}, "password": {"another-password"}}); r.StatusCode != 409 {
		t.Fatalf("重名：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/users/update", url.Values{"name": {"ci"}, "disabled": {"on"}, "manifest_rate": {"3"}}); r.StatusCode != 303 {
		t.Fatalf("修改：%d %s", r.StatusCode, r.body)
	}
	if u := e.app.Runtime().Users["ci"]; !u.Disabled || u.ManifestRPS != 3 {
		t.Fatalf("修改应生效：%+v", u)
	}
	if r := e.post(s, "/admin/users/delete", url.Values{"name": {"ci"}}); r.StatusCode != 303 || e.app.Runtime().Users["ci"] != nil {
		t.Fatalf("删除：%d", r.StatusCode)
	}

	// 管理员：不能删自己；改了别人的角色，那人的会话结束
	v := e.login("victor", victorPW)
	if r := e.post(s, "/admin/admins/delete", url.Values{"name": {"alice"}}); r.StatusCode != 403 {
		t.Fatalf("不能删除自己：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/update", url.Values{"name": {"victor"}, "role": {"admin"}}); r.StatusCode != 303 {
		t.Fatalf("改角色：%d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/", v.cookie); r.StatusCode != 303 {
		t.Fatalf("改了角色后旧会话结束：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"short"}, "role": {"viewer"}}); r.StatusCode != 400 {
		t.Fatalf("口令太短：%d", r.StatusCode)
	}

	// 改自己的口令：另一个浏览器的会话结束，当前浏览器换一个新会话
	other := e.login("alice", alicePW)
	r = e.post(s, "/admin/account/password", url.Values{"password": {"alice-new-password"}, "confirm": {"alice-new-password"}})
	if r.StatusCode != 303 || r.cookie(plainCookie) == nil {
		t.Fatalf("改口令：%d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/", other.cookie); r.StatusCode != 303 {
		t.Fatal("其他会话应结束")
	}
	if r := e.get("/admin/", plainCookie+"="+r.cookie(plainCookie).Value); r.StatusCode != 200 {
		t.Fatal("当前浏览器拿到新会话")
	}
	e.login("alice", "alice-new-password")
}

func TestAPIWithSession(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	if r := e.get(api.Prefix+"/status", s.cookie); r.StatusCode != 200 {
		t.Fatalf("会话能读 API：%d", r.StatusCode)
	}
	body := `{"cidr":"198.51.100.8","duration":"1h"}`
	call := func(hdr map[string]string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+api.Prefix+"/bans", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", s.cookie)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if c := call(map[string]string{"Origin": e.origin}); c != 403 {
		t.Fatalf("写操作没带 X-CSRF-Token：%d", c)
	}
	if c := call(map[string]string{"X-CSRF-Token": s.csrf}); c != 403 {
		t.Fatalf("写操作没带 Origin：%d", c)
	}
	if c := call(map[string]string{"X-CSRF-Token": s.csrf, "Origin": e.origin}); c != 201 {
		t.Fatalf("会话加 CSRF 可以写：%d", c)
	}
	if r := e.get(api.Prefix + "/status"); r.StatusCode != 401 {
		t.Fatalf("没有会话也没有令牌：%d", r.StatusCode)
	}
}

func TestReadOnlyStoreWithConfigAdmin(t *testing.T) {
	t.Setenv("T_ADMIN_PW", "config-admin-password")
	e := newEnv(t, envOpts{store: store.NewReadOnly(), yaml: "admin:\n  store: {driver: none}\n  initial_admin: {username: root, password: {env: T_ADMIN_PW}}\n"})
	s := e.login("root", "config-admin-password")
	page := e.get("/admin/users", s.cookie)
	if !strings.Contains(page.body, tr("zh", "readonly.banner")) || strings.Contains(page.body, `action="/admin/users/create"`) {
		t.Fatal("只读时显示提示，不显示表单")
	}
	if r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}); r.StatusCode != 409 || !strings.Contains(r.body, "只读") {
		t.Fatalf("只读时修改返回 409：%d", r.StatusCode)
	}
	if r := e.get("/admin/account", s.cookie); !strings.Contains(r.body, tr("zh", "account.fromConfig")) {
		t.Fatal("配置文件里的管理员要在配置里改口令")
	}
}

func TestNoAdmins(t *testing.T) {
	e := newEnv(t, envOpts{noAdm: true})
	if r := e.get("/admin/login"); !strings.Contains(r.body, tr("zh", "auth.noAdminsTitle")) {
		t.Fatal("没有管理员时登录页要说明怎么创建")
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                             "0 秒",
		45 * time.Second:              "45 秒",
		time.Hour:                     "1 小时",
		6*time.Hour + 5*time.Minute:   "6 小时 5 分钟",
		50*time.Hour + 30*time.Minute: "2 天 2 小时",
	} {
		if got := fmtDur("zh", d); got != want {
			t.Errorf("fmtDur(%s) = %q，应为 %q", d, got, want)
		}
	}
	if got := fmtDur("en", 90*time.Minute); got != "1 h 30 min" {
		t.Errorf("英文：%q", got)
	}
}

func TestInitialAdminMustChangePassword(t *testing.T) {
	t.Setenv("T_INIT_PW", "initial-admin-password")
	e := newEnv(t, envOpts{noAdm: true, yaml: "admin:\n  initial_admin: {username: root, password: {env: T_INIT_PW}}\n"})
	if a := e.app.Store.Data().Admins; len(a) != 1 || !a[0].MustChange {
		t.Fatalf("按 initial_admin 建的管理员必须先改口令：%+v", a)
	}
	r := e.do("POST", "/admin/login", url.Values{"username": {"root"}, "password": {"initial-admin-password"}, "next": {"/admin/rules"}}, nil, nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("登录后先去改口令：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	c := plainCookie + "=" + r.cookie(plainCookie).Value
	for _, p := range []string{"/admin/", "/admin/rules", "/admin/admins", "/admin/live"} {
		if r := e.get(p, c); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
			t.Errorf("%s 在改口令之前不能打开：%d %s", p, r.StatusCode, r.Header.Get("Location"))
		}
	}
	page := e.get("/admin/account", c)
	if page.StatusCode != 200 || !strings.Contains(page.body, tr("zh", "account.mustChangeTitle")) {
		t.Fatalf("我的账号页要说明原因：%d", page.StatusCode)
	}
	s := testSession{cookie: c, csrf: csrfRe.FindStringSubmatch(page.body)[1]}
	if r := e.post(s, "/admin/bans/add", url.Values{"cidr": {"198.51.100.7"}, "duration": {"1h"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("写操作也不行：%d", r.StatusCode)
	}
	if r := e.get(api.Prefix+"/status", c); r.StatusCode != 403 {
		t.Fatalf("API 也不能用会话访问：%d", r.StatusCode)
	}
	// 原样再设一遍不行
	if r := e.post(s, "/admin/account/password", url.Values{"password": {"initial-admin-password"}, "confirm": {"initial-admin-password"}}); r.StatusCode != 400 || !strings.Contains(r.body, "新口令不能和原来的一样") {
		t.Fatalf("不能沿用原来的口令：%d", r.StatusCode)
	}
	r = e.post(s, "/admin/account/password", url.Values{"password": {"root-own-password-1"}, "confirm": {"root-own-password-1"}})
	if r.StatusCode != 303 {
		t.Fatalf("改口令：%d %s", r.StatusCode, r.body)
	}
	c = plainCookie + "=" + r.cookie(plainCookie).Value
	if r := e.get("/admin/rules", c); r.StatusCode != 200 {
		t.Fatalf("改完口令后可以使用：%d", r.StatusCode)
	}
	if e.app.Store.Data().Admins[0].MustChange {
		t.Fatal("改完口令后不再要求")
	}
	// 配置里的旧口令不能再登录
	if r := e.do("POST", "/admin/login", url.Values{"username": {"root"}, "password": {"initial-admin-password"}}, nil, nil); r.StatusCode != 401 {
		t.Fatalf("旧口令应失效：%d", r.StatusCode)
	}
}

func TestPasswordsSetByOthersMustChange(t *testing.T) {
	e := newEnv(t, envOpts{})
	s := e.login("alice", alicePW)
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"carol"}, "role": {"viewer"}}); r.StatusCode != 400 {
		t.Fatalf("口令不能等于用户名：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/admins/create", url.Values{"name": {"carol"}, "password": {"carol-temp-password"}, "role": {"viewer"}}); r.StatusCode != 303 {
		t.Fatalf("新建管理员：%d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/admins", s.cookie); !strings.Contains(r.body, tr("zh", "admins.mustChange")) {
		t.Fatal("列表里要标出待改口令")
	}
	r := e.do("POST", "/admin/login", url.Values{"username": {"carol"}, "password": {"carol-temp-password"}}, nil, nil)
	if r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("别人建的账号第一次登录先改口令：%s", r.Header.Get("Location"))
	}
	// 重设别人的口令：对方也要先改
	if r := e.post(s, "/admin/admins/update", url.Values{"name": {"victor"}, "role": {"viewer"}, "password": {"victor-reset-password"}}); r.StatusCode != 303 {
		t.Fatalf("重设口令：%d", r.StatusCode)
	}
	if r := e.do("POST", "/admin/login", url.Values{"username": {"victor"}, "password": {"victor-reset-password"}}, nil, nil); r.Header.Get("Location") != "/admin/account" {
		t.Fatalf("被重设口令的管理员先改口令：%s", r.Header.Get("Location"))
	}
	// 改自己的口令不会触发
	if r := e.post(s, "/admin/account/password", url.Values{"password": {"alice-second-password"}, "confirm": {"alice-second-password"}}); r.StatusCode != 303 {
		t.Fatalf("改自己的口令：%d", r.StatusCode)
	}
	for _, a := range e.app.Store.Data().Admins {
		if a.Name == "alice" && a.MustChange {
			t.Fatal("自己改的口令不需要再改")
		}
	}
}

func TestConfigPages(t *testing.T) {
	e := newEnv(t, envOpts{yaml: "limits: {manifest_rate: 3}\n"})
	s := e.login("alice", alicePW)
	// 限流：只存和配置文件不同的字段
	form := url.Values{"key": {"limits"}}
	for _, f := range config.OverrideFields("limits") {
		form.Set(f, config.FieldText(e.app.Runtime().Config, "limits", f))
	}
	form.Set("max_inflight", "77")
	if r := e.post(s, "/admin/config/save", form); r.StatusCode != 303 || r.Header.Get("Location") != "/admin/config?ok=config.saved&v=limits" {
		t.Fatalf("保存：%d %s", r.StatusCode, r.body)
	}
	if v := e.app.Store.Data().ConfigOverrides["limits"].Value; v != "max_inflight: 77\n" || e.app.Runtime().Config.Limits.MaxInflight != 77 {
		t.Fatalf("存下的修改：%q", v)
	}
	r := e.get("/admin/config?ok=config.saved&v=limits", s.cookie)
	if !strings.Contains(r.body, "max_inflight: 77") || !strings.Contains(r.body, `action="/admin/config/reset"`) {
		t.Fatal("配置页列出修改，能重置")
	}
	checkPage(t, "config", r.body)
	// 填错：带着说明和填过的值重新显示，不保存
	form.Set("max_inflight", "lots")
	if r := e.post(s, "/admin/config/save", form); r.StatusCode != 400 || !strings.Contains(r.body, `value="lots"`) {
		t.Fatalf("填错：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/config/save", url.Values{"key": {"listen"}}); r.StatusCode != 400 {
		t.Fatalf("不能改的段：%d", r.StatusCode)
	}
	// 开关：没勾选按 false
	if r := e.post(s, "/admin/config/save", url.Values{"key": {"site"}}); r.StatusCode != 303 || e.app.Runtime().Site.ShowStatus {
		t.Fatalf("关掉服务状态：%d", r.StatusCode)
	}

	// 新建上游，再加一个固定转发到它的入口
	r = e.post(s, "/admin/config/upstream/save", url.Values{"new": {"1"}, "upstream": {"CR.example.com"}, "registry": {"registry.example.com"},
		"token_realm_hosts": {"auth.example.com"}, "unlisted_redirect": {"pass"}})
	if r.StatusCode != 303 || e.app.Runtime().Config.Upstreams["cr.example.com"] == nil {
		t.Fatalf("新建上游：%d %s", r.StatusCode, r.body)
	}
	if r := e.post(s, "/admin/config/upstream/save", url.Values{"new": {"1"}, "upstream": {"ghcr.io"}, "registry": {"x.example.com"}}); r.StatusCode != 409 {
		t.Fatalf("和已有的重名：%d", r.StatusCode)
	}
	if r := e.post(s, "/admin/config/upstream/save", url.Values{"new": {"1"}, "upstream": {"nodot"}}); r.StatusCode != 400 {
		t.Fatalf("名字不对：%d", r.StatusCode)
	}
	// 没写 token_realm_hosts：保存，但直接显示提醒
	r = e.post(s, "/admin/config/upstream/save", url.Values{"new": {"1"}, "upstream": {"bare.example.com"}, "registry": {"bare.example.com"}})
	if r.StatusCode != 200 || !strings.Contains(r.body, "token_realm_hosts") || !strings.Contains(r.body, "ol-alert-success") {
		t.Fatalf("有提醒时直接显示：%d", r.StatusCode)
	}
	checkPage(t, "upstream warnings", r.body)
	r = e.get("/admin/config/entrances", s.cookie)
	checkPage(t, "entrances", r.body)
	if !strings.Contains(r.body, `name="rows" value="3"`) {
		t.Fatal("一个入口加两行空的")
	}
	ent := url.Values{"rows": {"3"},
		"name_0": {"default"}, "hosts_0": {"*"}, "upstream_0": {""}, "default_upstream_0": {"docker.io"}, "auth_0": {"none"}, "page_0": {"public"},
		"name_1": {""}, "hosts_1": {"cr.test"}, "upstream_1": {"cr.example.com"}, "auth_1": {"none"}, "page_1": {"off"},
		"hosts_2": {""}}
	if r := e.post(s, "/admin/config/entrances/save", ent); r.StatusCode != 303 {
		t.Fatalf("保存入口：%d %s", r.StatusCode, r.body)
	}
	if en := e.app.Runtime().Router.Entrance("cr.test"); en == nil || en.Upstream != "cr.example.com" || en.Page != "off" {
		t.Fatalf("新入口应生效：%+v", en)
	}
	// 被入口引用的上游不能删
	if r := e.post(s, "/admin/config/reset", url.Values{"key": {"upstreams/cr.example.com"}}); r.StatusCode != 422 {
		t.Fatalf("被引用的上游不能删：%d", r.StatusCode)
	}
	// 删入口（勾选删除）
	ent.Set("delete_1", "1")
	if r := e.post(s, "/admin/config/entrances/save", ent); r.StatusCode != 303 || e.app.Runtime().Router.Entrance("cr.test").Upstream != "" {
		t.Fatalf("删除入口：%d", r.StatusCode)
	}
	if _, ok := e.app.Store.Data().ConfigOverrides["entrances"]; ok {
		t.Fatal("和配置文件一样的入口列表不存")
	}
	if r := e.post(s, "/admin/config/reset", url.Values{"key": {"upstreams/cr.example.com"}}); r.StatusCode != 303 || e.app.Runtime().Config.Upstreams["cr.example.com"] != nil {
		t.Fatalf("删除后台新建的上游：%d", r.StatusCode)
	}

	// 导出
	r = e.get("/admin/config/export", s.cookie)
	if r.StatusCode != 200 || !strings.Contains(r.body, "max_inflight: 77") || !strings.Contains(r.Header.Get("Content-Disposition"), "config-overrides.yaml") {
		t.Fatalf("导出：%d %s", r.StatusCode, r.body)
	}
	if r := e.get("/admin/config/export?format=merged", s.cookie); r.StatusCode != 200 || !strings.Contains(r.body, "manifest_rate: 3") {
		t.Fatalf("合并导出：%d %s", r.StatusCode, r.body)
	}

	// 只读角色：看得到，改不了
	v := e.login("victor", victorPW)
	r = e.get("/admin/config", v.cookie)
	if strings.Contains(r.body, `action="/admin/config/save"`) || !strings.Contains(r.body, "max_inflight") {
		t.Fatal("只读角色只能看")
	}
	if r := e.post(v, "/admin/config/save", url.Values{"key": {"limits"}, "max_inflight": {"1"}}); r.StatusCode != 403 {
		t.Fatalf("只读角色不能改：%d", r.StatusCode)
	}
	// 超过 10 分钟要重新输入口令
	e.advance(11 * time.Minute)
	if r := e.post(s, "/admin/config/save", form); r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/admin/reauth") {
		t.Fatalf("改配置要最近输入过口令：%d %s", r.StatusCode, r.Header.Get("Location"))
	}
}
