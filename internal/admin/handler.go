// Package admin 是网页管理后台：
//   - 全部页面在服务端用 html/template 渲染，没有脚本也能用；一个同源的小脚本只做本地时间、复制按钮、删除确认和概览刷新；
//   - 样式、字体、图标来自设计系统 @openlinkos/ui，打包进程序，不从第三方加载任何东西；
//   - 严格的 CSP（没有内联脚本和样式，Trusted Types 不允许任何策略）、会话、CSRF、来源检查、登录限速、
//     敏感操作要求最近输入过口令、表单提交成功后 303 跳转（PRG）。
//
// 校验和写入都在 internal/ops，和 JSON API 共用。
package admin

import (
	"bytes"
	"errors"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// CSP 是每个后台页面的内容安全策略（SVG 图片用下面更窄的那个）。
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

// SVGCSP：SVG 图片的颜色写在内嵌的 <style> 里，页面的策略会拦掉；它们单独用一个只允许内联样式的策略，
// 并且放进沙箱，直接打开也执行不了任何东西。
const SVGCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// Cookie 的名字。public_url 是 https 时会话 Cookie 用 __Host- 前缀（Secure、整个主机、SameSite=Lax）；
// 其他情况（本机、SSH 转发）用普通名字，只在 /admin 下、SameSite=Strict。
const (
	secureCookie = "__Host-relay-admin"
	plainCookie  = "relay-admin"
	langCookie   = "relay-admin-lang"
	themeCookie  = "relay-admin-theme"
	maxBody      = 64 << 10
	busyRetry    = 5 * time.Second
)

// Options 主要给测试用。
type Options struct {
	Log         *slog.Logger
	Now         func() time.Time
	LoginLimits *LoginLimits
	// BcryptCost 是这里生成哈希时的强度（只读模式下配置文件里的初始管理员），默认 ops.BcryptCost
	BcryptCost int
}

// LoginLimits 是口令校验失败的上限，默认每个客户端 10 分钟 10 次、全局 100 次。
type LoginLimits = loginLimits

// Handler 是 /admin/ 下除 JSON API 以外的全部请求。
type Handler struct {
	app      *app.App
	ops      *ops.Ops
	log      *slog.Logger
	now      func() time.Time
	assets   *assets
	tmpl     *template.Template
	sessions *sessions
	limiter  *loginLimiter
	queue    *checkQueue
	routes   []route
	cost     int

	cfgMu   sync.Mutex
	cfgKey  string // 只读模式：初始管理员口令的指纹 → 它的 bcrypt 哈希
	cfgHash []byte
}

type authLevel int

const (
	authNone    authLevel = iota
	authSession           // 登录即可
	authFresh             // 登录，并且 10 分钟内输入过口令
)

type route struct {
	method, path string
	auth         authLevel
	write        bool   // 要 admin 角色
	back         string // 要求重新输入口令的 POST：输完以后回到哪个页面
	run          func(c *ctx) error
}

// New 创建网页后台。
func New(a *app.App, o *ops.Ops, opt Options) (*Handler, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	limits := defaultLoginLimits
	if opt.LoginLimits != nil {
		limits = *opt.LoginLimits
	}
	if opt.BcryptCost == 0 {
		opt.BcryptCost = ops.BcryptCost
	}
	as, err := loadAssets()
	if err != nil {
		return nil, err
	}
	h := &Handler{app: a, ops: o, log: opt.Log, now: opt.Now, assets: as, sessions: newSessions(),
		limiter: newLoginLimiter(limits), queue: &checkQueue{max: 32}, cost: opt.BcryptCost}
	if h.tmpl, err = parseTemplates(); err != nil {
		return nil, err
	}
	h.routes = h.routeTable()
	// 管理员的口令、角色、停用状态改了，或者被删了：结束他的全部会话
	o.OnAdminChange = func(name string) { h.sessions.endFor(name, "") }
	return h, nil
}

// ctx 是一次请求。
type ctx struct {
	h      *Handler
	w      http.ResponseWriter
	r      *http.Request
	now    time.Time
	lang   string // zh | en
	theme  string // auto | light | dark
	sess   *session
	secure bool // 这次请求来自 https 的 public_url（决定会话 Cookie 的样式）
	ip     netip.Addr
	form   url.Values

	// 表单出错时重新渲染页面用
	status  int
	formErr string
	// 保存成功但有提醒时，不跳转，直接显示页面：成功提示和提醒
	flash    string
	warnings []string
}

// httpError 是要以错误页回答的请求错误。
type httpError struct {
	status int
	key    string // 界面文案的键
}

func (e *httpError) Error() string { return e.key }

func fail(status int, key string) error { return &httpError{status, key} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	for k, v := range securityHeaders(h.publicHTTPS()) {
		hd.Set(k, v)
	}
	if !ops.HostAllowed(h.app.Runtime().Config.Admin, r) {
		http.Error(w, "With admin.public: false the admin UI answers only at admin.public_url, localhost or an IP address. 管理后台只接受 admin.public_url、localhost 或 IP 访问。", http.StatusMisdirectedRequest)
		return
	}
	c := &ctx{h: h, w: w, r: r, now: h.now(), ip: h.clientAddr(r)}
	c.lang = negotiateLang(cookie(r, langCookie), r.Header.Get("Accept-Language"))
	switch t := cookie(r, themeCookie); t {
	case "light", "dark":
		c.theme = t
	default:
		c.theme = "auto"
	}
	if err := h.dispatch(c); err != nil {
		h.renderError(c, err)
	}
}

// securityHeaders 是 /admin 下每个响应都带的头。
func securityHeaders(https bool) map[string]string {
	m := map[string]string{
		"Content-Security-Policy": CSP,
		"Cache-Control":           "no-store, private",
		"Vary":                    "Cookie",
		"X-Content-Type-Options":  "nosniff",
		// 不用 no-referrer：那样浏览器在同源表单提交时会发 Origin: null，来源检查就失效了。
		// same-origin 仍然不会把来源发给别的站点。
		"Referrer-Policy":              "same-origin",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), bluetooth=(), hid=(), midi=()",
		"X-Robots-Tag":                 "noindex, nofollow",
	}
	if https {
		m["Strict-Transport-Security"] = "max-age=31536000"
	}
	return m
}

func (h *Handler) publicURL() string { return h.app.Runtime().Config.Admin.PublicURL }

func (h *Handler) publicOrigin() (origin string, https bool) {
	u, err := url.Parse(h.publicURL())
	if err != nil || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), u.Scheme == "https"
}

func (h *Handler) publicHTTPS() bool { _, s := h.publicOrigin(); return s }

// clientAddr 是请求方的地址：经过 client_ip.trusted_proxies 里的反向代理时取代理转来的地址。
func (h *Handler) clientAddr(r *http.Request) netip.Addr {
	if c, _ := h.app.Runtime().IPs.ClientIP(r); c.IsValid() {
		return c
	}
	return clientip.Remote(r)
}

func cookie(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

func (h *Handler) dispatch(c *ctx) error {
	r := c.r
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		c.w.Header().Set("Allow", "GET, POST")
		return fail(http.StatusMethodNotAllowed, "error.method")
	}
	p := r.URL.Path
	if p == "/admin" {
		return c.redirect("/admin/")
	}
	if file, ok := strings.CutPrefix(p, "/admin/assets/"); ok {
		if r.Method != http.MethodGet {
			c.w.Header().Set("Allow", "GET")
			return fail(http.StatusMethodNotAllowed, "error.method")
		}
		return h.serveAsset(c, file)
	}
	var rt *route
	var allowed []string
	for i := range h.routes {
		if h.routes[i].path == p {
			allowed = append(allowed, h.routes[i].method)
			if h.routes[i].method == r.Method {
				rt = &h.routes[i]
			}
		}
	}
	if rt == nil {
		if len(allowed) > 0 {
			c.w.Header().Set("Allow", strings.Join(allowed, ", "))
			return fail(http.StatusMethodNotAllowed, "error.method")
		}
		return fail(http.StatusNotFound, "error.notFound")
	}

	if r.Method == http.MethodPost {
		if err := h.checkOrigin(c); err != nil {
			return err
		}
		if err := c.parseForm(); err != nil {
			return err
		}
	} else if r.Header.Get("Sec-Fetch-Site") == "cross-site" && p == "/admin/live" {
		return fail(http.StatusForbidden, "error.crossSite")
	}

	c.sess = h.sessionFrom(r, c.now)
	if rt.auth == authNone {
		return rt.run(c)
	}
	if c.sess == nil {
		if r.Method == http.MethodGet {
			return c.redirect("/admin/login?next=" + url.QueryEscape(c.here()))
		}
		return c.redirect("/admin/login")
	}
	if r.Method == http.MethodPost {
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = c.form.Get("_csrf")
		}
		if !safeEqual(tok, c.sess.CSRF) {
			return fail(http.StatusForbidden, "error.csrf")
		}
	}
	// 口令是别人设的：改口令之前只能打开「我的账号」（和退出、确认身份）
	if c.sess.MustChange && !mustChangeAllowed[rt.path] {
		return c.redirect("/admin/account")
	}
	if rt.write && c.sess.Role != "admin" {
		return fail(http.StatusForbidden, "error.viewer")
	}
	if rt.auth == authFresh && !c.sess.fresh(c.now) {
		back := c.here()
		if r.Method == http.MethodPost {
			back = rt.back
		}
		return c.redirect("/admin/reauth?next=" + url.QueryEscape(safeNext(back)))
	}
	return rt.run(c)
}

// checkOrigin 是每个 POST 的跨站检查：拒绝 Sec-Fetch-Site: cross-site，并要求 Origin 是允许的来源之一：
// public_url 的来源，或者这次访问的 <协议>://<Host>（admin.public: false 时只限 localhost、IP）。
// 协议也认 https：经终止 TLS 的反向代理（CDN、Ingress）来的请求到这里是明文，浏览器那边是 https 页面；
// 同一个 Host 的 https 页面一定是这个站点自己的。Origin 是 https 时会话 Cookie 带 Secure。
// mustChangeAllowed 是必须先改口令的会话能打开的页面。
var mustChangeAllowed = map[string]bool{
	"/admin/account": true, "/admin/account/password": true, "/admin/logout": true, "/admin/reauth": true,
}

func (h *Handler) checkOrigin(c *ctx) error {
	r := c.r
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return fail(http.StatusForbidden, "error.crossSite")
	}
	origin := strings.ToLower(r.Header.Get("Origin"))
	if origin == "" {
		return fail(http.StatusForbidden, "error.noOrigin")
	}
	if pub, https := h.publicOrigin(); pub != "" && origin == pub {
		c.secure = https
		return nil
	}
	host := strings.ToLower(r.Host)
	if pub, https := h.publicOrigin(); https && pub == "https://"+host {
		return fail(http.StatusForbidden, "error.origin") // public_url 是 https 时，同一个域名的 http 页面不算
	}
	if !h.app.Runtime().Mgmt.Public && !ops.DirectHost(host) {
		return fail(http.StatusForbidden, "error.origin")
	}
	switch origin {
	case "https://" + host:
		c.secure = true
		return nil
	case "http://" + host:
		if r.TLS == nil {
			return nil
		}
	}
	return fail(http.StatusForbidden, "error.origin")
}

func (c *ctx) parseForm() error {
	if enc := c.r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return fail(http.StatusUnsupportedMediaType, "error.badForm")
	}
	mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if mt != "application/x-www-form-urlencoded" {
		if c.r.ContentLength == 0 && mt == "" {
			c.form = url.Values{}
			return nil
		}
		return fail(http.StatusUnsupportedMediaType, "error.badForm")
	}
	c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxBody)
	if err := c.r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fail(http.StatusRequestEntityTooLarge, "error.tooLarge")
		}
		return fail(http.StatusBadRequest, "error.badForm")
	}
	c.form = c.r.PostForm
	return nil
}

// sessionFrom 找请求带的会话。访问 https 的 public_url 时只认 __Host- Cookie：同一个上级域名下的兄弟站点
// 可以替这个主机种普通 Cookie，但种不了 __Host- 的。其他时候两种都认。
func (h *Handler) sessionFrom(r *http.Request, now time.Time) *session {
	names := []string{secureCookie, plainCookie}
	if pub, https := h.publicOrigin(); https && strings.EqualFold(strings.TrimPrefix(pub, "https://"), r.Host) {
		names = names[:1]
	}
	for _, n := range names {
		if s := h.sessions.get(cookie(r, n), now); s != nil {
			return s
		}
	}
	return nil
}

func sessionCookie(value string, secure bool, maxAge int) *http.Cookie {
	if secure {
		return &http.Cookie{Name: secureCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
	}
	// 明文 http 的页面：浏览器不接受 Secure Cookie（Safari），也没有意义
	return &http.Cookie{Name: plainCookie, Value: value, Path: "/admin", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge} //nolint:gosec // 见上
}

func (h *Handler) serveAsset(c *ctx, file string) error {
	f, ok := h.assets.byFile[file]
	if !ok {
		return fail(http.StatusNotFound, "error.notFound")
	}
	hd := c.w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("Content-Length", strconv.Itoa(len(f.body)))
	hd.Set("Cache-Control", "private, max-age=31536000, immutable")
	hd.Del("Vary")
	if strings.HasPrefix(f.ctype, "image/svg") {
		hd.Set("Content-Security-Policy", SVGCSP)
	}
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(f.body)
	return nil
}

// ---- 跳转

// here 是当前的路径和查询，用于 next 参数。
func (c *ctx) here() string {
	if c.r.URL.RawQuery != "" {
		return c.r.URL.Path + "?" + c.r.URL.RawQuery
	}
	return c.r.URL.Path
}

// redirect 是 303 跳转到后台里的一个地址。
func (c *ctx) redirect(to string) error {
	c.w.Header().Set("Location", to)
	c.w.WriteHeader(http.StatusSeeOther)
	return nil
}

// done 是表单成功后的跳转：带上一条成功提示（键在 notices 里登记过）和提示里的名字。
func (c *ctx) done(path, notice, name string) error {
	q := url.Values{"ok": {notice}}
	if name != "" {
		q.Set("v", name)
	}
	return c.redirect(path + "?" + q.Encode())
}

// safeNext 是 next 参数指向的地址：只能是后台里的页面，否则回到概览。不接受绝对地址、//、反斜杠（防开放跳转）。
func safeNext(next string) string {
	if next != "/admin" && !strings.HasPrefix(next, "/admin/") && !strings.HasPrefix(next, "/admin?") {
		return "/admin/"
	}
	if strings.ContainsAny(next, "\\ \t\r\n") || strings.Contains(next, "//") {
		return "/admin/"
	}
	if strings.HasPrefix(next, "/admin/api/") || strings.HasPrefix(next, "/admin/assets/") {
		return "/admin/"
	}
	return next
}

// ---- 身份

func (c *ctx) actor() ops.Actor {
	name := ""
	if c.sess != nil {
		name = c.sess.Admin
	}
	return ops.Actor{Kind: "admin", Name: name, IP: c.ip}
}

// account 找一个能登录的管理员：运营数据库里的；运营数据只读时，配置文件里的初始管理员。
func (h *Handler) account(name string) (hash []byte, role string, mustChange, ok bool) {
	for _, a := range h.app.Store.Data().Admins {
		if a.Name == name {
			if a.Disabled {
				return nil, "", false, false
			}
			return []byte(a.Hash), a.Role, a.MustChange, true
		}
	}
	ia := h.app.Runtime().Config.Admin.InitialAdmin
	if h.app.Store.Writable() || ia.Username == "" || ia.Username != name {
		return nil, "", false, false
	}
	// 只读模式：口令在配置里是明文（来自环境变量或文件），第一次用时算一次 bcrypt，口令变了再算
	pw := ia.Password.Reveal()
	key := hashID(pw)
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if h.cfgKey != key {
		hh, err := bcrypt.GenerateFromPassword([]byte(pw), h.cost)
		if err != nil {
			return nil, "", false, false
		}
		h.cfgKey, h.cfgHash = key, hh
	}
	// 运营数据只读时口令只能在配置里改，不要求在后台改
	return h.cfgHash, "admin", false, true
}

// dummyHash 让"用户不存在"和"口令错误"耗时相近。
var dummyHash = sync.OnceValue(func() []byte {
	b, _ := bcrypt.GenerateFromPassword([]byte("registry-mirrors-relay admin"), ops.BcryptCost)
	return b
})

type checkResult struct {
	ok         bool
	role       string
	mustChange bool
	status     int
	message    string // 界面文案的键
	wait       time.Duration
}

// checkPassword 校验管理员口令：排队一个一个地跑，受失败次数限制。失败在每个窗口、每个客户端只记两次审计
// （第一次失败、开始被拦）。
func (h *Handler) checkPassword(c *ctx, name, password, mode string) checkResult {
	client := c.ip.String()
	var res checkResult
	ran := h.queue.do(func() {
		if ok, wait, first := h.limiter.check(client, c.now); !ok {
			if first {
				h.ops.Audit(ops.Actor{Kind: "admin", Name: name, IP: c.ip}, mode+".blocked", name, "retry after "+wait.Round(time.Second).String())
			}
			res = checkResult{status: http.StatusTooManyRequests, message: "auth.tooMany", wait: wait}
			return
		}
		hash, role, mustChange, found := h.account(name)
		if !found || len(password) > 72 {
			_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		} else if bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil {
			res = checkResult{ok: true, role: role, mustChange: mustChange}
			return
		}
		if n := h.limiter.fail(client, c.now); n == 1 {
			h.ops.Audit(ops.Actor{Kind: "admin", Name: name, IP: c.ip}, mode+".failed", name, "")
		}
		res = checkResult{status: http.StatusUnauthorized, message: "auth.bad"}
	})
	if !ran {
		res = checkResult{status: http.StatusTooManyRequests, message: "auth.busy", wait: busyRetry}
	}
	if res.wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(res.wait.Seconds()+0.999)))
	}
	return res
}

// startSession 开始一个新会话（从不沿用请求带来的），设置 Cookie。
func (h *Handler) startSession(c *ctx, name, role string, mustChange bool) {
	if c.sess != nil {
		h.sessions.end(c.sess.idHash)
	}
	id, s := h.sessions.create(name, role, mustChange, c.ip.String(), c.r.UserAgent(), c.now)
	http.SetCookie(c.w, sessionCookie(id, c.secure, 0))
	c.sess = s
}

func (h *Handler) endSession(c *ctx) {
	if c.sess != nil {
		h.sessions.end(c.sess.idHash)
	}
	http.SetCookie(c.w, sessionCookie("", true, -1))
	http.SetCookie(c.w, sessionCookie("", false, -1))
	c.sess = nil
}

// Session 让 JSON API 也认网页后台的会话。写操作要带 X-CSRF-Token，并通过和表单一样的来源检查。
func (h *Handler) Session(r *http.Request) (ops.Actor, string, bool, error) {
	now := h.now()
	s := h.sessionFrom(r, now)
	if s == nil {
		return ops.Actor{}, "", false, nil
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return ops.Actor{}, "", false, errors.New("不接受跨站请求")
	}
	if s.MustChange {
		return ops.Actor{}, "", false, errors.New("请先在管理后台修改口令")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		c := &ctx{h: h, r: r}
		if err := h.checkOrigin(c); err != nil {
			return ops.Actor{}, "", false, errors.New("请求来源不对")
		}
		if !safeEqual(r.Header.Get("X-CSRF-Token"), s.CSRF) {
			return ops.Actor{}, "", false, errors.New("缺少或错误的 X-CSRF-Token")
		}
	}
	return ops.Actor{Kind: "admin", Name: s.Admin, IP: h.clientAddr(r)}, s.Role, true, nil
}

// ---- 渲染

// render 渲染一个页面。
func (h *Handler) render(c *ctx, page string, v *view) error {
	v.c, v.h = c, h
	if v.Notice == "" {
		v.Notice = c.notice()
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "page/"+page, v); err != nil {
		return err
	}
	v.Body = template.HTML(buf.String()) //nolint:gosec // html/template 刚渲染出来的，已经转义过
	buf.Reset()
	if err := h.tmpl.ExecuteTemplate(&buf, "layout", v); err != nil {
		return err
	}
	body := h.assets.withSprite(buf.Bytes())
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
	return nil
}

// renderError 以错误页回答。ops 的错误和请求错误给出说明，其他错误是 500，细节只写进日志。
func (h *Handler) renderError(c *ctx, err error) {
	status, msg := http.StatusInternalServerError, ""
	var he *httpError
	var oe *ops.Error
	switch {
	case errors.As(err, &he):
		status, msg = he.status, tr(c.lang, he.key)
	case errors.As(err, &oe):
		status, msg = oe.Status, oe.Message(c.lang)
	default:
		h.log.Error("管理后台出错", "path", c.r.URL.Path, "err", err)
		msg = tr(c.lang, "error.internalText")
	}
	c.status = status
	v := &view{Title: errorTitle(c.lang, status), Data: map[string]any{"Status": status, "Message": msg}}
	if rerr := h.render(c, "error", v); rerr != nil {
		h.log.Error("渲染错误页失败", "err", rerr)
		http.Error(c.w, msg, status)
	}
}

func errorTitle(lang string, status int) string {
	switch {
	case status == http.StatusNotFound:
		return tr(lang, "error.notFoundTitle")
	case status == http.StatusForbidden:
		return tr(lang, "error.forbiddenTitle")
	case status == http.StatusTooManyRequests:
		return tr(lang, "error.tooManyTitle")
	case status >= 500:
		return tr(lang, "error.internalTitle")
	}
	return tr(lang, "error.badRequestTitle")
}

// retry 处理表单的错误：ops 的错误（输入不对、重名、只读等）就带着说明和填过的值重新渲染 page，其他错误交上去。
func (c *ctx) retry(err error, page func(*ctx) error) error {
	var oe *ops.Error
	if !errors.As(err, &oe) {
		return err
	}
	c.status, c.formErr = oe.Status, oe.Message(c.lang)
	return page(c)
}

// Version 是页面页脚显示的版本。
func Version() string { return version.Version }
