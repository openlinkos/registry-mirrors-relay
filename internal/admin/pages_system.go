package admin

import (
	"cmp"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
)

// ---- The usage page: what a browser sees at an entrance's domain

type siteData struct {
	Site      ops.SiteView
	LinkKeys  []string
	Brands    []ui.Brand
	Status    cfgSection    // site.show_status, a configuration override
	Entrances []entranceRow // and the usage page each one shows
	PageURL   string        // the usage page of the first entrance that has a public one
}

func (h *Handler) sitePage(c *ctx) error {
	rt := h.app.Runtime()
	d := siteData{Site: h.ops.Site(), LinkKeys: ops.SiteLinkKeys, Brands: ui.Brands(), Status: h.section(c, "site")}
	scheme := "https"
	if pub, https := h.publicOrigin(); pub != "" && !https {
		scheme = "http"
	}
	for i, e := range rt.Config.Entrances {
		d.Entrances = append(d.Entrances, rowOf(i, e))
		if d.PageURL == "" && e.Page == "public" {
			for _, host := range e.Hosts {
				if host != "*" {
					d.PageURL = scheme + "://" + host + "/"
					break
				}
			}
		}
	}
	return h.render(c, "site", &view{Title: tr(c.lang, "nav.site"), Nav: "site", Data: d})
}

func (h *Handler) siteSave(c *ctx) error {
	in := ops.SiteInput{Name: c.form.Get("name"), Operator: c.form.Get("operator"), Brand: c.form.Get("brand"), Links: map[string]string{}}
	for _, k := range ops.SiteLinkKeys {
		in.Links[k] = c.form.Get("link_" + k)
	}
	if err := h.ops.SetSite(c.actor(), in); err != nil {
		return c.retry(err, h.sitePage)
	}
	return c.done("/admin/site", "site.saved", "")
}

func (h *Handler) siteReset(c *ctx) error {
	if err := h.ops.ResetSite(c.actor()); err != nil {
		return c.retry(err, h.sitePage)
	}
	return c.done("/admin/site", "site.reset", "")
}

// ---- Administrators, tokens, login sessions

type adminsData struct {
	Admins   []ops.AdminView
	Tokens   []ops.TokenView
	Sessions []session
	Current  string // the handle of the current session
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
		Crumbs: []crumb{{tr(c.lang, "nav.admins"), "/admin/admins"}}, Data: secretData{Kind: "token", Name: name, Secret: tok, Back: "/admin/admins", Base: h.base(c.r)}})
}

// base is the address the browser uses to reach the console (scheme plus Host), for the example
// commands. When Host is the public_url domain, public_url is used.
func (h *Handler) base(r *http.Request) string {
	if pub, _ := h.publicOrigin(); pub != "" {
		if u, err := url.Parse(pub); err == nil && strings.EqualFold(u.Host, r.Host) {
			return pub
		}
	}
	return requestBase(r)
}

// requestBase is the address of this request. Requests through a TLS-terminating reverse proxy arrive
// here in plain text, so X-Forwarded-Proto decides; it is only for display and has no security role.
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host}).String()
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

// ---- System

type systemData struct {
	Version, Go, ConfigPath, StoreDriver, StorePath string
	Started, Built                                  time.Time
	Uptime                                          string
	MemLimit, HeapInuse, Sys                        uint64
	Goroutines, Procs                               int
	Writable                                        bool
	Reload                                          *app.ReloadStatus
	Dropped                                         uint64
	Log                                             config.Log
	// Management entrance (console, API, metrics)
	MgmtAddr, PullAddr  string // an empty MgmtAddr means the port is shared with the pull entrance
	Allow, ProxiedAllow string
	Public              bool // the console and API are open to all addresses (admin.public)
	Metrics, Pprof      bool
}

func (h *Handler) systemPage(c *ctx) error {
	rt := h.app.Runtime()
	cfg := rt.Config
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	d := systemData{
		Version: Version(), Go: runtime.Version(), ConfigPath: h.app.ConfigPath(), StoreDriver: cfg.Admin.Store.Driver,
		StorePath: cfg.Admin.Store.Path,
		Started:   h.app.Started, Built: rt.Built, Uptime: fmtDur(c.lang, c.now.Sub(h.app.Started)),
		HeapInuse: ms.HeapInuse, Sys: ms.Sys, Goroutines: runtime.NumGoroutine(), Procs: runtime.GOMAXPROCS(0),
		Writable: h.app.Store.Writable(), Reload: h.app.LastReload(), Dropped: h.app.AccessLogDropped(),
		MgmtAddr: cfg.Listen.Admin, PullAddr: cmp.Or(cfg.Listen.HTTP, cfg.Listen.HTTPS),
		Allow: strings.Join(cfg.Admin.Allow, ", "), ProxiedAllow: strings.Join(cfg.Admin.ProxiedAllow, ", "), Public: cfg.Admin.Public,
		Metrics: cfg.Metrics.Enabled, Pprof: cfg.Metrics.Enabled && cfg.Metrics.Pprof,
	}
	if l := debug.SetMemoryLimit(-1); l > 0 && l < 1<<62 {
		d.MemLimit = uint64(l)
	}
	d.Log = cfg.Log
	return h.render(c, "system", &view{Title: tr(c.lang, "nav.system"), Nav: "system", Data: d})
}

func (h *Handler) reload(c *ctx) error {
	if _, err := h.ops.Reload(c.actor()); err != nil {
		return c.retry(err, h.systemPage)
	}
	return c.done("/admin/system", "reloaded", "")
}

// ---- Own account

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
	// Changing the password ends all of this administrator's sessions (OnAdminChange); a new one is then
	// opened here for the current browser
	if err := h.ops.UpdateAdmin(c.actor(), name, ops.AdminInput{Password: &pw}); err != nil {
		return c.retry(err, h.accountPage)
	}
	h.startSession(c, name, c.sess.Role, false)
	return c.done("/admin/account", "password.changed", "")
}
