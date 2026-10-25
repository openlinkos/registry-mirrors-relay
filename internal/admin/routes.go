package admin

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
)

// routeTable is every route of the console: only GET and POST, and every write is a POST. This table is
// everything the console can do; the tests pin down the writes one by one, so any extra one is noticed.
// The console does not touch docker.sock, does not run commands and cannot send requests to arbitrary
// addresses (it can only probe the upstreams in the configuration).
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
		// Configuration changes: entrances and upstreams decide where pull requests go, so like
		// administrators and tokens they require a recently entered password
		fresh("/admin/config/save", "/admin/config", h.configSave),
		fresh("/admin/config/entrances/save", "/admin/config/entrances", h.entrancesSave),
		fresh("/admin/config/upstream/save", "/admin/config", h.upstreamSave),
		fresh("/admin/config/reset", "/admin/config", h.configReset),
		get("/admin/system", h.systemPage),
		post("/admin/reload", h.reload),
		get("/admin/account", h.accountPage),
		// Changing one's own password: the read-only role can do it too, but needs a recently entered
		// password
		{method: http.MethodPost, path: "/admin/account/password", auth: authFresh, back: "/admin/account", run: h.passwordChange},
	}
}

// WriteRoutes lists every POST route (the tests use it to pin down the console's writes).
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

// ---- Login, logout, re-authentication, preferences

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

// prefs stores the interface language and colour scheme (cookies) and needs no login: switching the
// language has to work on the login page too.
func (h *Handler) prefs(c *ctx) error {
	const year = 365 * 24 * 3600
	set := func(name, value string) {
		// Secure can only be set with an https public_url, as with the session cookie
		http.SetCookie(c.w, &http.Cookie{Name: name, Value: value, Path: "/admin", MaxAge: year, HttpOnly: true, //nolint:gosec // see above
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
	}
	if l, ok := i18n.Parse(c.form.Get("lang")); ok {
		// The usage page reads this cookie too, so it lives at / and is not HttpOnly: it is only a preference.
		http.SetCookie(c.w, &http.Cookie{Name: langCookie, Value: string(l), Path: "/", MaxAge: year, //nolint:gosec // see above: a preference, not HttpOnly
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
		http.SetCookie(c.w, &http.Cookie{Name: oldLangCookie, Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode}) //nolint:gosec // deletes the old cookie; Secure as above
	}
	if t := c.form.Get("theme"); t == "auto" || t == "light" || t == "dark" {
		set(themeCookie, t)
	}
	return c.redirect(safeNext(c.form.Get("next")))
}

// notices are the success notices that can be shown after a PRG redirect: ?ok=<key>&v=<name>. Only the
// keys registered here are accepted.
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

// fmtDur writes a duration as "2 d 3 h" or "45 min", taking only the two largest units.
func fmtDur(lang i18n.Lang, d time.Duration) string {
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

// ago is how long ago t was, for things like "last used".
func ago(lang i18n.Lang, now, t time.Time) string {
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
