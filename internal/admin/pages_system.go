package admin

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// ---- 设置

type settingsData struct {
	Site     ops.SiteView
	LinkKeys []string
	Log      config.Log
}

func (h *Handler) settingsPage(c *ctx) error {
	d := settingsData{Site: h.ops.Site(), LinkKeys: ops.SiteLinkKeys, Log: h.app.Runtime().Config.Log}
	return h.render(c, "settings", &view{Title: tr(c.lang, "nav.settings"), Nav: "settings", Data: d})
}

func (h *Handler) siteSave(c *ctx) error {
	in := ops.SiteInput{Name: c.form.Get("name"), Operator: c.form.Get("operator"), Links: map[string]string{}}
	for _, k := range ops.SiteLinkKeys {
		in.Links[k] = c.form.Get("link_" + k)
	}
	if err := h.ops.SetSite(c.actor(), in); err != nil {
		return c.retry(err, h.settingsPage)
	}
	return c.done("/admin/settings", "site.saved", "")
}

func (h *Handler) siteReset(c *ctx) error {
	if err := h.ops.ResetSite(c.actor()); err != nil {
		return c.retry(err, h.settingsPage)
	}
	return c.done("/admin/settings", "site.reset", "")
}

// ---- 管理员、令牌、登录会话

type adminsData struct {
	Admins   []ops.AdminView
	Tokens   []ops.TokenView
	Sessions []session
	Current  string // 当前会话的标识
}

func (h *Handler) adminsPage(c *ctx) error {
	d := adminsData{Admins: h.ops.Admins(), Tokens: h.ops.Tokens(), Sessions: h.sessions.list(c.now), Current: c.sess.Handle}
	return h.render(c, "admins", &view{Title: tr(c.lang, "nav.admins"), Nav: "admins", Data: d})
}

func (h *Handler) adminPage(c *ctx) error {
	name := c.r.URL.Query().Get("name")
	if name == "" {
		name = c.form.Get("name")
	}
	for _, a := range h.ops.Admins() {
		if a.Name == name {
			return h.render(c, "admin", &view{Title: a.Name, Nav: "admins", Crumbs: []crumb{{tr(c.lang, "nav.admins"), "/admin/admins"}}, Data: a})
		}
	}
	return fail(http.StatusNotFound, "error.notFound")
}

func (h *Handler) adminCreate(c *ctx) error {
	name := strings.TrimSpace(c.form.Get("name"))
	if err := h.ops.CreateAdmin(c.actor(), name, c.form.Get("password"), c.form.Get("role")); err != nil {
		return c.retry(err, h.adminsPage)
	}
	return c.done("/admin/admins", "admin.created", name)
}

func (h *Handler) adminUpdate(c *ctx) error {
	name := c.form.Get("name")
	role := c.form.Get("role")
	disabled := c.form.Get("disabled") == "on"
	in := ops.AdminInput{Role: &role, Disabled: &disabled}
	if pw := c.form.Get("password"); pw != "" {
		in.Password = &pw
	}
	if err := h.ops.UpdateAdmin(c.actor(), name, in); err != nil {
		return c.retry(err, h.adminPage)
	}
	return c.done("/admin/admins", "admin.updated", name)
}

func (h *Handler) adminDelete(c *ctx) error {
	name := c.form.Get("name")
	if err := h.ops.DeleteAdmin(c.actor(), name); err != nil {
		return c.retry(err, h.adminsPage)
	}
	return c.done("/admin/admins", "admin.deleted", name)
}

func (h *Handler) tokenCreate(c *ctx) error {
	name := strings.TrimSpace(c.form.Get("name"))
	tok, err := h.ops.CreateToken(c.actor(), name, c.form.Get("role"))
	if err != nil {
		return c.retry(err, h.adminsPage)
	}
	return h.render(c, "secret", &view{Title: tr(c.lang, "secret.tokenTitle"), Nav: "admins",
		Crumbs: []crumb{{tr(c.lang, "nav.admins"), "/admin/admins"}}, Data: secretData{Kind: "token", Name: name, Secret: tok, Back: "/admin/admins"}})
}

func (h *Handler) tokenDelete(c *ctx) error {
	name := c.form.Get("name")
	if err := h.ops.DeleteToken(c.actor(), name); err != nil {
		return c.retry(err, h.adminsPage)
	}
	return c.done("/admin/admins", "token.deleted", name)
}

func (h *Handler) sessionRevoke(c *ctx) error {
	handle := c.form.Get("handle")
	if handle == c.sess.Handle {
		return fail(http.StatusBadRequest, "admins.revokeSelf")
	}
	s, ok := h.sessions.endHandle(handle)
	if !ok {
		return fail(http.StatusNotFound, "error.notFound")
	}
	h.ops.Audit(c.actor(), "session.revoke", s.Admin, s.Handle)
	return c.done("/admin/admins", "session.revoked", s.Admin)
}

// ---- 系统

type systemData struct {
	Version, Go, ConfigPath, StoreDriver, StorePath, Admin, Metrics string
	Started, Built                                                  time.Time
	Uptime                                                          string
	MemLimit, HeapInuse, Sys                                        uint64
	Goroutines, Procs                                               int
	Writable, Pprof                                                 bool
	Reload                                                          *app.ReloadStatus
	Dropped                                                         uint64
	Config                                                          string
}

func (h *Handler) systemPage(c *ctx) error {
	rt := h.app.Runtime()
	cfg := rt.Config
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	d := systemData{
		Version: Version(), Go: runtime.Version(), ConfigPath: h.app.ConfigPath(), StoreDriver: cfg.Admin.Store.Driver,
		StorePath: cfg.Admin.Store.Path, Admin: cfg.Listen.Admin, Metrics: cfg.Listen.Metrics,
		Started: h.app.Started, Built: rt.Built, Uptime: fmtDur(c.lang, c.now.Sub(h.app.Started)),
		HeapInuse: ms.HeapInuse, Sys: ms.Sys, Goroutines: runtime.NumGoroutine(), Procs: runtime.GOMAXPROCS(0),
		Writable: h.app.Store.Writable(), Pprof: cfg.Metrics.Pprof, Reload: h.app.LastReload(), Dropped: h.app.AccessLogDropped(),
	}
	if l := debug.SetMemoryLimit(-1); l > 0 && l < 1<<62 {
		d.MemLimit = uint64(l)
	}
	if b, err := cfg.Print(); err == nil {
		d.Config = string(b)
	}
	return h.render(c, "system", &view{Title: tr(c.lang, "nav.system"), Nav: "system", Data: d})
}

func (h *Handler) reload(c *ctx) error {
	if _, err := h.ops.Reload(c.actor()); err != nil {
		return c.retry(err, h.systemPage)
	}
	return c.done("/admin/system", "reloaded", "")
}

// ---- 自己的账号

func (h *Handler) accountPage(c *ctx) error {
	var me *ops.AdminView
	for _, a := range h.ops.Admins() {
		if a.Name == c.sess.Admin {
			me = &a
			break
		}
	}
	return h.render(c, "account", &view{Title: tr(c.lang, "nav.account"), Data: me})
}

func (h *Handler) passwordChange(c *ctx) error {
	pw := c.form.Get("password")
	if pw != c.form.Get("confirm") {
		c.status, c.formErr = http.StatusBadRequest, tr(c.lang, "account.mismatch")
		return h.accountPage(c)
	}
	name := c.sess.Admin
	// 改了口令后这个管理员的会话全部结束（OnAdminChange），这里再给当前浏览器开一个新的
	if err := h.ops.UpdateAdmin(c.actor(), name, ops.AdminInput{Password: &pw}); err != nil {
		return c.retry(err, h.accountPage)
	}
	h.startSession(c, name, c.sess.Role)
	return c.done("/admin/account", "password.changed", "")
}
