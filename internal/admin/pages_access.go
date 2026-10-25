package admin

import (
	"net/http"
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

func (h *Handler) usersPage(c *ctx) error {
	return h.render(c, "users", &view{Title: tr(c.lang, "nav.users"), Nav: "users", Data: h.ops.Users()})
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
	Base   string // the console's current address, e.g. http://127.0.0.1:8080, for the API examples
}

func (h *Handler) userCreate(c *ctx) error {
	name := strings.TrimSpace(c.form.Get("name"))
	pw := c.form.Get("password")
	generated := pw == ""
	if generated {
		pw = ops.GeneratePassword()
	}
	note := c.form.Get("note")
	rate, streams, err := limitsFrom(c)
	if err == nil {
		err = h.ops.CreateUser(c.actor(), ops.UserInput{Name: name, Password: &pw, Note: &note, ManifestRate: rate, BlobStreams: streams})
	}
	if err != nil {
		return c.retry(err, h.usersPage)
	}
	if generated {
		return h.render(c, "secret", &view{Title: tr(c.lang, "secret.passwordTitle"), Nav: "users",
			Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}, Data: secretData{Kind: "password", Name: name, Secret: pw, Back: "/admin/users"}})
	}
	return c.done("/admin/users", "user.created", name)
}

type userData struct {
	User ops.UserView
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
	return h.render(c, "user", &view{Title: u.Name, Nav: "users", Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}, Data: userData{User: u}})
}

func (h *Handler) userUpdate(c *ctx) error {
	name := c.form.Get("name")
	disabled := c.form.Get("disabled") == "on"
	note := c.form.Get("note")
	in := ops.UserInput{Disabled: &disabled, Note: &note}
	var err error
	in.ManifestRate, in.BlobStreams, err = limitsFrom(c)
	pw := c.form.Get("password")
	generated := c.form.Get("generate") == "on"
	if generated {
		pw = ops.GeneratePassword()
	}
	if pw != "" {
		in.Password = &pw
	}
	if err == nil {
		err = h.ops.UpdateUser(c.actor(), name, in)
	}
	if err != nil {
		return c.retry(err, h.userPage)
	}
	if generated {
		return h.render(c, "secret", &view{Title: tr(c.lang, "secret.passwordTitle"), Nav: "users",
			Crumbs: []crumb{{tr(c.lang, "nav.users"), "/admin/users"}}, Data: secretData{Kind: "password", Name: name, Secret: pw, Back: "/admin/users"}})
	}
	return c.done("/admin/users", "user.updated", name)
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
	Overrides []ops.UserView
	Limited   []limitedClient
}

func (h *Handler) limitsPage(c *ctx) error {
	d := limitsData{Limits: h.app.Runtime().Config.Limits, Stats: h.app.Limiter.Stats()}
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
