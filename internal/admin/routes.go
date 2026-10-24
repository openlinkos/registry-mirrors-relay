package admin

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

// routeTable 是后台的全部路由：只有 GET 和 POST，每个写操作都是 POST。这张表就是后台的全部能力，
// 测试把写操作逐个固定下来，多出来的会被发现。后台不碰 docker.sock、不执行命令，也不能向任意地址发请求
// （只能探测配置里的上游）。
func (h *Handler) routeTable() []route {
	get := func(path string, run func(*ctx) error) route {
		return route{method: http.MethodGet, path: path, auth: authSession, run: run}
	}
	post := func(path string, run func(*ctx) error) route {
		return route{method: http.MethodPost, path: path, auth: authSession, write: true, run: run}
	}
	fresh := func(path, back string, run func(*ctx) error) route {
		return route{method: http.MethodPost, path: path, auth: authFresh, write: true, back: back, run: run}
	}
	return []route{
		{method: http.MethodGet, path: "/admin/login", auth: authNone, run: h.loginPage},
		{method: http.MethodPost, path: "/admin/login", auth: authNone, run: h.login},
		{method: http.MethodPost, path: "/admin/prefs", auth: authNone, run: h.prefs},
		{method: http.MethodPost, path: "/admin/logout", auth: authSession, run: h.logout},
		get("/admin/reauth", h.reauthPage),
		{method: http.MethodPost, path: "/admin/reauth", auth: authSession, run: h.reauth},

		get("/admin/", h.overviewPage),
		get("/admin/live", h.live),
		get("/admin/upstreams", h.upstreamsPage),
		post("/admin/upstreams/check", h.upstreamCheck),
		post("/admin/upstreams/hosts/add", h.redirectHostAdd),
		post("/admin/upstreams/hosts/delete", h.redirectHostDelete),
		get("/admin/usage", h.usagePage),
		get("/admin/users", h.usersPage),
		get("/admin/users/edit", h.userPage),
		post("/admin/users/create", h.userCreate),
		post("/admin/users/update", h.userUpdate),
		post("/admin/users/delete", h.userDelete),
		get("/admin/rules", h.rulesPage),
		post("/admin/rules/ip/add", h.ipRuleAdd),
		post("/admin/rules/ip/delete", h.ipRuleDelete),
		post("/admin/bans/add", h.banAdd),
		post("/admin/bans/delete", h.banDelete),
		post("/admin/rules/image/add", h.imageRuleAdd),
		post("/admin/rules/image/delete", h.imageRuleDelete),
		get("/admin/limits", h.limitsPage),
		get("/admin/requests", h.requestsPage),
		get("/admin/abuse", h.abusePage),
		get("/admin/audit", h.auditPage),
		get("/admin/settings", h.settingsPage),
		post("/admin/settings/site", h.siteSave),
		post("/admin/settings/reset", h.siteReset),
		get("/admin/admins", h.adminsPage),
		get("/admin/admins/edit", h.adminPage),
		fresh("/admin/admins/create", "/admin/admins", h.adminCreate),
		fresh("/admin/admins/update", "/admin/admins", h.adminUpdate),
		fresh("/admin/admins/delete", "/admin/admins", h.adminDelete),
		fresh("/admin/tokens/create", "/admin/admins", h.tokenCreate),
		fresh("/admin/tokens/delete", "/admin/admins", h.tokenDelete),
		fresh("/admin/sessions/revoke", "/admin/admins", h.sessionRevoke),
		get("/admin/config", h.configPage),
		get("/admin/config/export", h.configExport),
		get("/admin/config/entrances", h.entrancesPage),
		get("/admin/config/upstream", h.upstreamEditPage),
		// 改配置：入口和上游决定拉取请求发到哪里，和管理员、令牌一样要最近输入过口令
		fresh("/admin/config/save", "/admin/config", h.configSave),
		fresh("/admin/config/entrances/save", "/admin/config/entrances", h.entrancesSave),
		fresh("/admin/config/upstream/save", "/admin/config", h.upstreamSave),
		fresh("/admin/config/reset", "/admin/config", h.configReset),
		get("/admin/system", h.systemPage),
		post("/admin/reload", h.reload),
		get("/admin/account", h.accountPage),
		// 改自己的口令：只读角色也可以，但要最近输入过口令
		{method: http.MethodPost, path: "/admin/account/password", auth: authFresh, back: "/admin/account", run: h.passwordChange},
	}
}

// WriteRoutes 列出所有 POST 路由（测试用来固定后台的写操作）。
func (h *Handler) WriteRoutes() []string {
	var out []string
	for _, r := range h.routes {
		if r.method == http.MethodPost {
			out = append(out, r.path)
		}
	}
	slices.Sort(out)
	return out
}

// ---- 登录、退出、重新输入口令、偏好

type loginData struct {
	Next, Username string
	NoAdmins       bool
}

func (h *Handler) noAdmins() bool {
	return len(h.ops.Admins()) == 0
}

func (h *Handler) loginPage(c *ctx) error {
	if c.sess != nil {
		return c.redirect(safeNext(c.r.URL.Query().Get("next")))
	}
	return h.render(c, "login", &view{Title: tr(c.lang, "auth.signIn"), Bare: true,
		Data: loginData{Next: safeNext(c.r.URL.Query().Get("next")), NoAdmins: h.noAdmins()}})
}

func (h *Handler) login(c *ctx) error {
	name := strings.TrimSpace(c.form.Get("username"))
	next := safeNext(c.form.Get("next"))
	res := h.checkPassword(c, name, c.form.Get("password"), "login")
	if !res.ok {
		c.status, c.formErr = res.status, tr(c.lang, res.message, "min", int(res.wait.Minutes()+0.999))
		return h.render(c, "login", &view{Title: tr(c.lang, "auth.signIn"), Bare: true,
			Data: loginData{Next: next, Username: name, NoAdmins: h.noAdmins()}})
	}
	h.startSession(c, name, res.role, res.mustChange)
	h.ops.Audit(c.actor(), "login", name, "")
	if res.mustChange {
		return c.redirect("/admin/account")
	}
	return c.redirect(next)
}

func (h *Handler) logout(c *ctx) error {
	if c.sess != nil {
		h.ops.Audit(c.actor(), "logout", c.sess.Admin, "")
	}
	h.endSession(c)
	return c.redirect("/admin/login")
}

type reauthData struct{ Next string }

func (h *Handler) reauthPage(c *ctx) error {
	return h.render(c, "reauth", &view{Title: tr(c.lang, "auth.reauthTitle"), Bare: true,
		Data: reauthData{Next: safeNext(c.r.URL.Query().Get("next"))}})
}

func (h *Handler) reauth(c *ctx) error {
	next := safeNext(c.form.Get("next"))
	res := h.checkPassword(c, c.sess.Admin, c.form.Get("password"), "reauth")
	if !res.ok {
		c.status, c.formErr = res.status, tr(c.lang, res.message, "min", int(res.wait.Minutes()+0.999))
		return h.render(c, "reauth", &view{Title: tr(c.lang, "auth.reauthTitle"), Bare: true, Data: reauthData{Next: next}})
	}
	h.sessions.markFresh(c.sess.idHash, c.now)
	h.ops.Audit(c.actor(), "reauth", c.sess.Admin, "")
	return c.redirect(next)
}

// prefs 记下界面语言和配色（Cookie），不需要登录：语言切换在登录页上也要能用。
func (h *Handler) prefs(c *ctx) error {
	const year = 365 * 24 * 3600
	set := func(name, value string) {
		// 只有 https 的 public_url 时才能加 Secure，和会话 Cookie 一样
		http.SetCookie(c.w, &http.Cookie{Name: name, Value: value, Path: "/admin", MaxAge: year, HttpOnly: true, //nolint:gosec // 见上
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
	}
	if l := c.form.Get("lang"); l == "zh" || l == "en" {
		set(langCookie, l)
	}
	if t := c.form.Get("theme"); t == "auto" || t == "light" || t == "dark" {
		set(themeCookie, t)
	}
	return c.redirect(safeNext(c.form.Get("next")))
}

// notices 是 PRG 跳转后可以显示的成功提示：?ok=<键>&v=<名字>。只认这里登记的键。
var notices = []string{
	"user.created", "user.updated", "user.deleted", "rule.added", "rule.deleted", "ban.added", "ban.deleted",
	"host.added", "host.deleted", "site.saved", "site.reset", "admin.created", "admin.updated", "admin.deleted",
	"token.deleted", "session.revoked", "reloaded", "password.changed", "upstream.ok", "upstream.failed",
	"config.saved", "config.reset",
}

func (c *ctx) notice() string {
	if c.flash != "" {
		return c.flash
	}
	q := c.r.URL.Query()
	k := q.Get("ok")
	if k == "" || !slices.Contains(notices, k) {
		return ""
	}
	return tr(c.lang, "notice."+k, "v", q.Get("v"))
}

// fmtDur 把时长写成"2 天 3 小时""45 分钟"这样，只取最大的两个单位。
func fmtDur(lang string, d time.Duration) string {
	d = max(d, 0)
	days, hours, mins := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour), int(d%time.Hour/time.Minute)
	part := func(key string, n int) string { return tr(lang, key, "n", n) }
	switch {
	case days > 0 && hours > 0:
		return part("dur.days", days) + " " + part("dur.hours", hours)
	case days > 0:
		return part("dur.days", days)
	case hours > 0 && mins > 0:
		return part("dur.hours", hours) + " " + part("dur.minutes", mins)
	case hours > 0:
		return part("dur.hours", hours)
	case mins > 0:
		return part("dur.minutes", mins)
	}
	return part("dur.seconds", int(d/time.Second))
}

// ago 是现在距 t 多久，用于"最后使用"之类。
func ago(lang string, now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return tr(lang, "time.justNow")
	case d < time.Hour:
		return tr(lang, "time.minutesAgo", "n", int(d.Minutes()))
	case d < 48*time.Hour:
		return tr(lang, "time.hoursAgo", "n", int(d.Hours()))
	}
	return tr(lang, "time.daysAgo", "n", int(d.Hours()/24))
}
