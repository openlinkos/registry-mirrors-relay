package admin

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/limit"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
)

// ---- Client accounts

type usersData struct {
	Users     []ops.UserView
	Total     int
	Q, Status string // the filter: a name or note to find, and active or disabled
}

func (h *Handler) usersPage(c *ctx) error {
	q := c.r.URL.Query()
	d := usersData{Q: strings.TrimSpace(q.Get("q")), Status: q.Get("status")}
	all := h.ops.Users()
	d.Total = len(all)
	needle := strings.ToLower(d.Q)
	for _, u := range all {
		switch {
		case d.Status == "active" && u.Disabled, d.Status == "disabled" && !u.Disabled:
			continue
		case needle != "" && !strings.Contains(strings.ToLower(u.Name), needle) && !strings.Contains(strings.ToLower(u.Note), needle):
			continue
		}
		d.Users = append(d.Users, u)
	}
	return h.render(c, "users", &view{Title: tr(c.lang, "nav.users"), Nav: "users", Data: d})
}

// loginHosts are the host names of the entrances that need sign-in: where docker login goes.
func (h *Handler) loginHosts() []string {
	var out []string
	for _, e := range h.app.Runtime().Config.Entrances {
		if e.Auth != "basic" {
			continue
		}
		for _, host := range e.Hosts {
			if host != "*" && !slices.Contains(out, host) {
				out = append(out, host)
			}
		}
	}
	return out
}

// limitsFrom reads the per-account limits from the form: empty or 0 means the default.
func limitsFrom(c *ctx) (rate *float64, streams *int, err error) {
	r, s := 0.0, 0
	if v := strings.TrimSpace(c.form.Get("manifest_rate")); v != "" {
		if r, err = strconv.ParseFloat(v, 64); err != nil {
			return nil, nil, ops.NewError(http.StatusBadRequest, "limit.manifest_rate")
		}
	}
	if v := strings.TrimSpace(c.form.Get("blob_streams")); v != "" {
		if s, err = strconv.Atoi(v); err != nil {
			return nil, nil, ops.NewError(http.StatusBadRequest, "limit.blob_streams")
		}
	}
	return &r, &s, nil
}

type secretData struct {
	Kind   string // password | token
	Name   string
	Secret string
	Back   string
	Base   string   // the console's current address, e.g. http://127.0.0.1:8080, for the API examples
	Hosts  []string // the entrances that need sign-in, for docker login
}

func (h *Handler) userNewPage(c *ctx) error {
	return h.render(c, "userNew", &view{Title: tr(c.lang, "users.create"), Nav: "users", Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}})
}

// userCreate creates an account and shows how to sign in with it, with the password when it was
// generated (shown this once).
func (h *Handler) userCreate(c *ctx) error {
	name := strings.TrimSpace(c.form.Get("name"))
	pw := c.form.Get("password")
	generated := c.form.Get("generate") != "" || pw == ""
	if generated {
		pw = ops.GeneratePassword()
	}
	note := c.form.Get("note")
	rate, streams, err := limitsFrom(c)
	if err == nil {
		err = h.ops.CreateUser(c.actor(), ops.UserInput{Name: name, Password: &pw, Note: &note, ManifestRate: rate, BlobStreams: streams})
	}
	if err != nil {
		return c.retry(err, h.userNewPage)
	}
	d := secretData{Kind: "account", Name: name, Back: "/admin/users/edit?name=" + url.QueryEscape(name), Hosts: h.loginHosts()}
	if generated {
		d.Secret = pw
	}
	return h.render(c, "secret", &view{Title: tr(c.lang, "secret.accountTitle", "name", name), Nav: "users",
		Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}, Data: d})
}

type userData struct {
	User     ops.UserView
	Requests []stats.Request // the account's latest requests
	Hosts    []string        // the entrances that need sign-in
}

func (h *Handler) userPage(c *ctx) error {
	name := c.r.URL.Query().Get("name")
	if name == "" {
		name = c.form.Get("name")
	}
	u, ok := h.ops.User(name)
	if !ok {
		return fail(http.StatusNotFound, "error.notFound")
	}
	d := userData{User: u, Requests: h.app.Stats.Recent(stats.Filter{User: u.Name}, 12), Hosts: h.loginHosts()}
	return h.render(c, "user", &view{Title: u.Name, Nav: "users", Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}, Data: d})
}

// userUpdate saves one part of an account's page: its limits and note (part=limits), or a new password
// (part=password, typed or generated).
func (h *Handler) userUpdate(c *ctx) error {
	name := c.form.Get("name")
	var in ops.UserInput
	generated := false
	var err error
	switch c.form.Get("part") {
	case "password":
		pw := c.form.Get("password")
		if generated = c.form.Get("generate") != "" || pw == ""; generated {
			pw = ops.GeneratePassword()
		}
		in.Password = &pw
	default:
		note := c.form.Get("note")
		in.Note = &note
		in.ManifestRate, in.BlobStreams, err = limitsFrom(c)
	}
	if err == nil {
		err = h.ops.UpdateUser(c.actor(), name, in)
	}
	if err != nil {
		return c.retry(err, h.userPage)
	}
	back := "/admin/users/edit?name=" + url.QueryEscape(name)
	if generated {
		return h.render(c, "secret", &view{Title: tr(c.lang, "secret.passwordTitle"), Nav: "users",
			Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}, {name, back}},
			Data:   secretData{Kind: "password", Name: name, Secret: *in.Password, Back: back, Hosts: h.loginHosts()}})
	}
	return c.done(back, "user.updated", name)
}

// userToggle disables or enables an account; next is the page to go back to.
func (h *Handler) userToggle(c *ctx) error {
	name := c.form.Get("name")
	disabled := c.form.Get("disabled") == "true"
	if err := h.ops.UpdateUser(c.actor(), name, ops.UserInput{Disabled: &disabled}); err != nil {
		return c.retry(err, h.usersPage)
	}
	notice := "user.enabled"
	if disabled {
		notice = "user.disabled"
	}
	return c.done(safeNext(c.form.Get("next")), notice, name)
}

func (h *Handler) userDelete(c *ctx) error {
	name := c.form.Get("name")
	if err := h.ops.DeleteUser(c.actor(), name); err != nil {
		return c.retry(err, h.usersPage)
	}
	return c.done("/admin/users", "user.deleted", name)
}

// ---- Rules

type rulesData struct {
	ops.Rules
	Test      *ops.ImageDecision
	TestImage string
	BanCIDR   string // filled in beforehand when arriving from another page via "Ban"
	Upstreams []string
}

func (h *Handler) rulesPage(c *ctx) error {
	q := c.r.URL.Query()
	d := rulesData{Rules: h.ops.Rules(), BanCIDR: q.Get("ban"), Upstreams: h.app.Runtime().Pool.Names()}
	if img := strings.TrimSpace(q.Get("image")); img != "" {
		t := h.ops.TestImage(img)
		d.Test, d.TestImage = &t, img
	}
	return h.render(c, "rules", &view{Title: tr(c.lang, "nav.rules"), Nav: "rules", Data: d})
}

func (h *Handler) ipRuleAdd(c *ctx) error {
	cidr, err := h.ops.AddIPRule(c.actor(), c.form.Get("cidr"), c.form.Get("kind"), c.form.Get("note"))
	if err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "rule.added", cidr)
}

func (h *Handler) ipRuleDelete(c *ctx) error {
	cidr := c.form.Get("cidr")
	if err := h.ops.DeleteIPRule(c.actor(), cidr, c.form.Get("kind")); err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "rule.deleted", cidr)
}

func (h *Handler) banAdd(c *ctx) error {
	cidr, _, err := h.ops.AddBan(c.actor(), c.form.Get("cidr"), c.form.Get("duration"), c.form.Get("reason"))
	if err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "ban.added", cidr)
}

func (h *Handler) banDelete(c *ctx) error {
	cidr := c.form.Get("cidr")
	if err := h.ops.DeleteBan(c.actor(), cidr); err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "ban.deleted", cidr)
}

func (h *Handler) imageRuleAdd(c *ctx) error {
	status, _ := strconv.Atoi(c.form.Get("status"))
	p, err := h.ops.AddImageRule(c.actor(), ops.ImageRuleInput{Pattern: c.form.Get("pattern"), Kind: c.form.Get("kind"),
		Status: status, Message: c.form.Get("message"), Note: c.form.Get("note")})
	if err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "rule.added", p)
}

func (h *Handler) imageRuleDelete(c *ctx) error {
	p := c.form.Get("pattern")
	if err := h.ops.DeleteImageRule(c.actor(), p, c.form.Get("kind")); err != nil {
		return c.retry(err, h.rulesPage)
	}
	return c.done("/admin/rules", "rule.deleted", p)
}

// ---- Rate limits

type limitedClient struct {
	Client, User string
	Count        int
	Last         time.Time
}

type limitsData struct {
	Limits    config.Limits
	Stats     limit.Stats
	Settings  cfgSection // the limits, edited here
	Overrides []ops.UserView
	Limited   []limitedClient
}

func (h *Handler) limitsPage(c *ctx) error {
	d := limitsData{Limits: h.app.Runtime().Config.Limits, Stats: h.app.Limiter.Stats(), Settings: h.section(c, "limits")}
	for _, u := range h.ops.Users() {
		if u.ManifestRate > 0 || u.BlobStreams > 0 {
			d.Overrides = append(d.Overrides, u)
		}
	}
	// Recently rate-limited clients: find the 429s among recent requests and group them by client
	by := map[string]*limitedClient{}
	for _, r := range h.app.Stats.Recent(stats.Filter{Status: http.StatusTooManyRequests}, 5000) {
		k := r.ClientIP + " " + r.User
		lc, ok := by[k]
		if !ok {
			lc = &limitedClient{Client: r.ClientIP, User: r.User, Last: r.Time}
			by[k] = lc
		}
		lc.Count++
	}
	for _, lc := range by {
		d.Limited = append(d.Limited, *lc)
	}
	sort.Slice(d.Limited, func(i, j int) bool { return d.Limited[i].Count > d.Limited[j].Count })
	if len(d.Limited) > 50 {
		d.Limited = d.Limited[:50]
	}
	return h.render(c, "limits", &view{Title: tr(c.lang, "nav.limits"), Nav: "limits", Data: d})
}
