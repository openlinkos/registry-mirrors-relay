// Package admin is the web admin console:
//   - every page is rendered on the server with html/template and works without scripts; one small
//     same-origin script only handles local times, copy buttons, delete confirmations and overview refresh;
//   - styles, fonts and icons come from the design system @openlinkos/ui, are bundled into the program,
//     and nothing is loaded from third parties;
//   - a strict CSP (no inline scripts or styles, Trusted Types allows no policy), sessions, CSRF, origin
//     checks, login rate limits, sensitive operations require a recently entered password, and a
//     successful form submission ends in a 303 redirect (PRG).
//
// Validation and writes live in internal/ops and are shared with the JSON API.
package admin

import (
	"bytes"
	"cmp"
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
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// CSP is the content security policy of every console page (SVG images use the narrower one below).
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

// SVGCSP: the colours of the SVG images are written in an embedded <style>, which the page policy would
// block; they get a separate policy that allows only inline styles, and are sandboxed, so nothing can run
// even when one is opened directly.
const SVGCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// Cookie names. When public_url is https the session cookie uses the __Host- prefix (Secure, the whole
// host, SameSite=Lax); otherwise (local machine, SSH forwarding) it uses a plain name, only under /admin,
// SameSite=Strict.
const (
	secureCookie  = "__Host-relay-admin"
	plainCookie   = "relay-admin"
	langCookie    = "relay-lang" // shared with the usage page, so Path=/ and readable by scripts
	oldLangCookie = "relay-admin-lang"
	themeCookie   = "relay-admin-theme"
	maxBody       = 64 << 10
	busyRetry     = 5 * time.Second
)

// Options are mainly for tests.
type Options struct {
	Log         *slog.Logger
	Now         func() time.Time
	LoginLimits *LoginLimits
	// BcryptCost is the cost of hashes generated here (the initial administrator from the configuration
	// file in read-only mode); defaults to ops.BcryptCost
	BcryptCost int
}

// LoginLimits caps failed password checks; by default 10 per client and 100 overall in 10 minutes.
type LoginLimits = loginLimits

// Handler serves every request under /admin/ except the JSON API.
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
	cfgKey  string // read-only mode: fingerprint of the initial administrator's password → its bcrypt hash
	cfgHash []byte
}

type authLevel int

const (
	authNone    authLevel = iota
	authSession           // being logged in is enough
	authFresh             // logged in, and the password was entered within the last 10 minutes
)

type route struct {
	method, path string
	auth         authLevel
	write        bool   // requires the admin role
	back         string // POSTs that require re-authentication: the page to return to afterwards
	run          func(c *ctx) error
}

// New creates the web console.
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
	// An administrator's password, role or disabled state changed, or they were deleted: end all their
	// sessions
	o.OnAdminChange = func(name string) { h.sessions.endFor(name, "") }
	return h, nil
}

// ctx is one request.
type ctx struct {
	h      *Handler
	w      http.ResponseWriter
	r      *http.Request
	now    time.Time
	lang   i18n.Lang
	theme  string // auto | light | dark
	sess   *session
	secure bool // this request came from the https public_url (decides the session cookie's form)
	ip     netip.Addr
	form   url.Values

	// For re-rendering the page when a form has an error
	status  int
	formErr string
	// When a save succeeds but comes with warnings, the page is shown directly instead of redirecting:
	// the success notice and the warnings
	flash    string
	warnings []string
}

// httpError is a request error to be answered with the error page.
type httpError struct {
	status int
	key    string // key of the interface text
}

func (e *httpError) Error() string { return e.key }

func fail(status int, key string) error { return &httpError{status, key} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	for k, v := range securityHeaders(h.publicHTTPS()) {
		hd.Set(k, v)
	}
	lang := Lang(r)
	if !ops.HostAllowed(h.app.Runtime().Config.Admin, r) {
		http.Error(w, i18n.T(lang, "With admin.public: false, the console answers only at admin.public_url, localhost or an IP address."),
			http.StatusMisdirectedRequest)
		return
	}
	c := &ctx{h: h, w: w, r: r, now: h.now(), ip: h.clientAddr(r), lang: lang}
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

// securityHeaders are the headers every response under /admin carries.
func securityHeaders(https bool) map[string]string {
	m := map[string]string{
		"Content-Security-Policy": CSP,
		"Cache-Control":           "no-store, private",
		"Vary":                    "Cookie",
		"X-Content-Type-Options":  "nosniff",
		// Not no-referrer: with it, browsers send Origin: null on same-origin form submissions and the
		// origin check stops working. same-origin still sends no referrer to other sites.
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

// clientAddr is the requester's address: when the request comes through a reverse proxy listed in
// client_ip.trusted_proxies, the address the proxy forwards is used.
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
	// HEAD is answered like GET (net/http drops the body), for monitoring probes
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		c.w.Header().Set("Allow", "GET, HEAD, POST")
		return fail(http.StatusMethodNotAllowed, "error.method")
	}
	p := r.URL.Path
	if p == "/admin" {
		return c.redirect("/admin/")
	}
	if file, ok := strings.CutPrefix(p, "/admin/assets/"); ok {
		if method != http.MethodGet {
			c.w.Header().Set("Allow", "GET, HEAD")
			return fail(http.StatusMethodNotAllowed, "error.method")
		}
		return h.serveAsset(c, file)
	}
	var rt *route
	var allowed []string
	for i := range h.routes {
		if h.routes[i].path == p {
			allowed = append(allowed, h.routes[i].method)
			if h.routes[i].method == method {
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

	if method == http.MethodPost {
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
		if method == http.MethodGet {
			return c.redirect("/admin/login?next=" + url.QueryEscape(c.here()))
		}
		return c.redirect("/admin/login")
	}
	if method == http.MethodPost {
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = c.form.Get("_csrf")
		}
		if !safeEqual(tok, c.sess.CSRF) {
			return fail(http.StatusForbidden, "error.csrf")
		}
	}
	// The password was set by someone else: until it is changed, only "My account" can be opened (plus
	// logging out and confirming identity)
	if c.sess.MustChange && !mustChangeAllowed[rt.path] {
		return c.redirect("/admin/account")
	}
	if rt.write && c.sess.Role != "admin" {
		return fail(http.StatusForbidden, "error.viewer")
	}
	if rt.auth == authFresh && !c.sess.fresh(c.now) {
		back := c.here()
		if method == http.MethodPost {
			back = rt.back
		}
		return c.redirect("/admin/reauth?next=" + url.QueryEscape(safeNext(back)))
	}
	return rt.run(c)
}

// mustChangeAllowed are the pages a session that must change its password first can open.
var mustChangeAllowed = map[string]bool{
	"/admin/account": true, "/admin/account/password": true, "/admin/logout": true, "/admin/reauth": true,
}

// checkOrigin is the cross-site check for every POST: it rejects Sec-Fetch-Site: cross-site and requires
// Origin to be one of the allowed origins: the origin of public_url, or <scheme>://<Host> of this request
// (only localhost and IP addresses when admin.public: false).
// The https scheme is accepted too: requests through a TLS-terminating reverse proxy (CDN, Ingress) arrive
// here in plain text while the browser shows an https page; an https page on the same Host must belong to
// this site. When Origin is https the session cookie is Secure.
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
		return fail(http.StatusForbidden, "error.origin") // with an https public_url, http pages on that domain do not count
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

// sessionFrom finds the session the request carries. On the https public_url only the __Host- cookie is
// accepted: a sibling site under the same parent domain can plant a plain cookie for this host, but not a
// __Host- one. Otherwise both are accepted.
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
	// Plain-text http pages: browsers do not accept a Secure cookie there (Safari), and it would be
	// pointless anyway
	return &http.Cookie{Name: plainCookie, Value: value, Path: "/admin", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge} //nolint:gosec // see above
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

// ---- Redirects

// here is the current path and query, for the next parameter.
func (c *ctx) here() string {
	if c.r.URL.RawQuery != "" {
		return c.r.URL.Path + "?" + c.r.URL.RawQuery
	}
	return c.r.URL.Path
}

// redirect is a 303 redirect to an address in the console.
func (c *ctx) redirect(to string) error {
	c.w.Header().Set("Location", to)
	c.w.WriteHeader(http.StatusSeeOther)
	return nil
}

// done is the redirect after a successful form: it carries a success notice (a key registered in
// notices) and the name used in the notice.
func (c *ctx) done(path, notice, name string) error {
	q := url.Values{"ok": {notice}}
	if name != "" {
		q.Set("v", name)
	}
	return c.redirect(path + "?" + q.Encode())
}

// safeNext is the address the next parameter points to: it can only be a page in the console, otherwise
// it falls back to the overview. Absolute URLs, // and backslashes are not accepted (against open
// redirects).
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

// ---- Identity

func (c *ctx) actor() ops.Actor {
	name := ""
	if c.sess != nil {
		name = c.sess.Admin
	}
	return ops.Actor{Kind: "admin", Name: name, IP: c.ip}
}

// account finds an administrator who can log in: one in the operational database, or, when the
// operational data is read-only, the initial administrator from the configuration file.
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
	// Read-only mode: the password is in plain text in the configuration (from an environment variable
	// or a file); bcrypt is computed on first use and again when the password changes
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
	// When the operational data is read-only the password can only be changed in the configuration, so
	// changing it in the console is not required
	return h.cfgHash, "admin", false, true
}

// dummyHash makes "no such user" and "wrong password" take about the same time.
var dummyHash = sync.OnceValue(func() []byte {
	b, _ := bcrypt.GenerateFromPassword([]byte("registry-mirrors-relay admin"), ops.BcryptCost)
	return b
})

type checkResult struct {
	ok         bool
	role       string
	mustChange bool
	status     int
	message    string // key of the interface text
	wait       time.Duration
}

// checkPassword checks an administrator's password: checks run one at a time in a queue and are limited
// by the number of failures. Failures are written to the audit log only twice per window and client (the
// first failure, and when blocking starts).
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

// startSession starts a new session (never reusing the one the request carries) and sets the cookie.
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

// Session lets the JSON API accept web console sessions too. Writes must carry X-CSRF-Token and pass the
// same origin check as forms.
func (h *Handler) Session(r *http.Request) (ops.Actor, string, bool, error) {
	now := h.now()
	s := h.sessionFrom(r, now)
	if s == nil {
		return ops.Actor{}, "", false, nil
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return ops.Actor{}, "", false, i18n.Coded("auth.cross_site", "Cross-site requests are not accepted.")
	}
	if s.MustChange {
		return ops.Actor{}, "", false, i18n.Coded("auth.must_change", "Change your password in the console first.")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		c := &ctx{h: h, r: r}
		if err := h.checkOrigin(c); err != nil {
			return ops.Actor{}, "", false, i18n.Coded("auth.origin", "The request comes from the wrong origin.")
		}
		if !safeEqual(r.Header.Get("X-CSRF-Token"), s.CSRF) {
			return ops.Actor{}, "", false, i18n.Coded("auth.csrf", "X-CSRF-Token is missing or wrong.")
		}
	}
	return ops.Actor{Kind: "admin", Name: s.Admin, IP: h.clientAddr(r)}, s.Role, true, nil
}

// ---- Rendering

// render renders a page.
func (h *Handler) render(c *ctx, page string, v *view) error {
	v.c, v.h = c, h
	if v.Notice == "" {
		v.Notice = c.notice()
	}
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "page/"+page, v); err != nil {
		return err
	}
	v.Body = template.HTML(buf.String()) //nolint:gosec // just rendered by html/template, already escaped
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

// renderError answers with the error page. ops errors and request errors come with an explanation; other
// errors are a 500 and the details go only to the log.
func (h *Handler) renderError(c *ctx, err error) {
	status, msg := http.StatusInternalServerError, ""
	var he *httpError
	var oe *ops.Error
	switch {
	case errors.As(err, &he):
		status, msg = he.status, tr(c.lang, he.key)
	case errors.As(err, &oe):
		status, msg = oe.Status, i18n.Localize(err, c.lang)
	default:
		h.log.Error("admin console error", "path", c.r.URL.Path, "err", err)
		msg = tr(c.lang, "error.internalText")
	}
	c.status = status
	v := &view{Title: errorTitle(c.lang, status), Data: map[string]any{"Status": status, "Message": msg}}
	if rerr := h.render(c, "error", v); rerr != nil {
		h.log.Error("render error page failed", "err", rerr)
		http.Error(c.w, msg, status)
	}
}

func errorTitle(lang i18n.Lang, status int) string {
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

// retry handles form errors: for an ops error (invalid input, duplicate name, read-only and so on) it
// re-renders page with the explanation and the values already filled in; other errors are passed up.
func (c *ctx) retry(err error, page func(*ctx) error) error {
	var oe *ops.Error
	if !errors.As(err, &oe) {
		return err
	}
	c.status, c.formErr = oe.Status, i18n.Localize(err, c.lang)
	return page(c)
}

// Version is the version shown in the page footer.
func Version() string { return version.Version }

// Lang is the interface language of a request: the language cookie (shared with the usage page), then
// Accept-Language, then English. The JSON API uses it too.
func Lang(r *http.Request) i18n.Lang {
	return i18n.Negotiate(cmp.Or(cookie(r, langCookie), cookie(r, oldLangCookie)), r.Header.Get("Accept-Language"))
}
