package admin

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// ---- Configuration: changes made in the console are stored in the operational database and layered
// over the configuration file (see config.Override)

// cfgField is one field in the form.
type cfgField struct {
	Name, Label, Hint string
	Kind              string   // bool int float duration size list string
	Value             string   // the effective value; the value filled in when a form error is redisplayed
	File              string   // the value in the configuration file
	Changed           bool     // the effective value differs from the file's (changed in the console)
	Locked            bool     // an upstream with credentials can only be changed in the configuration file
	Choices           []string // the value must be one of these
}

type cfgSection struct {
	Key, Title, Help string
	Fields           []cfgField
	Override         *ops.OverrideView
}

// ledgerRow is one change made in the console, with what it is and where it is edited.
type ledgerRow struct {
	ops.OverrideView
	Title, Edit string
}

type configData struct {
	Editable  bool // can save: admin role, operational data writable, and no admin.config_edit: false
	EditOff   bool // admin.config_edit: false
	Ledger    []ledgerRow
	Sections  []cfgSection // the settings edited on this page
	Effective string       // the effective configuration (YAML, secrets hidden)
}

// globalSections are edited on the configuration page; the other sections on the page that uses them.
var globalSections = []string{"cache", "proxy"}

// ledgerOf says what an override is and where it is edited.
func ledgerOf(lang i18n.Lang, o ops.OverrideView) ledgerRow {
	r := ledgerRow{OverrideView: o}
	switch name, isUpstream := strings.CutPrefix(o.Key, "upstreams/"); {
	case isUpstream:
		r.Title, r.Edit = tr(lang, "config.ledgerUpstream", "name", name), "/admin/upstreams/edit?name="+url.QueryEscape(name)
	case o.Key == "entrances":
		r.Title, r.Edit = tr(lang, "nav.entrances"), "/admin/entrances"
	case o.Key == "limits":
		r.Title, r.Edit = tr(lang, "nav.limits"), "/admin/limits"
	case o.Key == "site":
		r.Title, r.Edit = tr(lang, "nav.site"), "/admin/site#status"
	default:
		r.Title, r.Edit = tr(lang, "cfg."+o.Key), "/admin/config#"+o.Key
	}
	return r
}

// section is one section of settings with its fields, for the page that edits it. A section whose form is
// being shown again after an error keeps what was entered.
func (h *Handler) section(c *ctx, key string) cfgSection {
	rt := h.app.Runtime()
	var form url.Values
	if c.form != nil && c.form.Get("key") == key {
		form = c.form
	}
	return cfgSection{Key: key, Title: tr(c.lang, "cfg."+key), Help: tr(c.lang, "cfg."+key+"Help"),
		Fields: fields(c.lang, rt.Config, rt.FileConfig, key, form), Override: overrideOf(h.ops.Overrides(), key)}
}

// fieldChoices are the fields whose value must be one of a few choices.
var fieldChoices = map[string][]string{
	"unlisted_redirect": {"pass", "deny"},
	"auth":              {"none", "basic"},
	"page":              {"public", "login", "off"},
}

func (h *Handler) canEditConfig(c *ctx) bool {
	return c.sess != nil && c.sess.Role == "admin" && h.ops.ConfigEditable()
}

func overrideOf(list []ops.OverrideView, key string) *ops.OverrideView {
	for i := range list {
		if list[i].Key == key {
			return &list[i]
		}
	}
	return nil
}

// fields lists the editable fields of section key: eff is the effective configuration, file the
// configuration file's. When form is not nil (a form error is being redisplayed) the filled-in values are used.
func fields(lang i18n.Lang, eff, file *config.Config, key string, form url.Values) []cfgField {
	sec, _, _ := strings.Cut(key, "/")
	locked, inFile := false, true
	if sec == "upstreams" {
		u := file.Upstreams[strings.TrimPrefix(key, "upstreams/")]
		locked, inFile = u != nil && u.Credentials != nil, u != nil
	}
	var out []cfgField
	for _, name := range config.OverrideFields(key) {
		f := cfgField{
			Name: name, Kind: config.FieldKind(key, name), Choices: fieldChoices[name],
			Label: tr(lang, "cfg."+sec+"."+name), Hint: tr(lang, "cfg."+sec+"."+name+"Hint"),
			Value: config.FieldText(eff, key, name), File: config.FieldText(file, key, name),
			Locked: locked && config.CredentialLocked(name),
		}
		if f.Hint == "cfg."+sec+"."+name+"Hint" {
			f.Hint = ""
		}
		f.Changed = inFile && f.Value != f.File // upstreams created in the console have no file value to compare
		if form != nil {
			switch {
			case f.Kind == "bool":
				f.Value = strconv.FormatBool(form.Get(name) != "")
			case form.Has(name):
				f.Value = form.Get(name)
			}
		}
		out = append(out, f)
	}
	return out
}

func (h *Handler) configPage(c *ctx) error {
	rt := h.app.Runtime()
	d := configData{Editable: h.canEditConfig(c), EditOff: !rt.FileConfig.Admin.ConfigEdit}
	for _, o := range h.ops.Overrides() {
		d.Ledger = append(d.Ledger, ledgerOf(c.lang, o))
	}
	for _, key := range globalSections {
		d.Sections = append(d.Sections, h.section(c, key))
	}
	if b, err := rt.Config.Print(); err == nil {
		d.Effective = string(b)
	}
	return h.render(c, "config", &view{Title: tr(c.lang, "nav.config"), Nav: "config", Data: d})
}

// saveConfig saves one override. On success it redirects back to back; when there are new warnings it
// shows page directly and lists them.
func (h *Handler) saveConfig(c *ctx, key, value, back string, page func(*ctx) error) error {
	warnings, err := h.ops.SetConfig(c.actor(), key, value)
	if err != nil {
		return c.retry(err, page)
	}
	if len(warnings) == 0 {
		return c.done(back, "config.saved", key)
	}
	c.form, c.warnings, c.flash = nil, localize(c.lang, warnings), tr(c.lang, "notice.config.saved", "v", key)
	return page(c)
}

func badConfig(err error) error {
	e := ops.NewError(http.StatusBadRequest, "config.invalid")
	e.Cause = err
	return e
}

// formValues takes the fields of section key from the form. Browsers do not send an unticked switch, so
// it counts as false.
func formValues(form url.Values, key string, skip func(string) bool) map[string]string {
	out := map[string]string{}
	for _, name := range config.OverrideFields(key) {
		switch {
		case skip != nil && skip(name):
		case config.FieldKind(key, name) == "bool":
			out[name] = strconv.FormatBool(form.Get(name) != "")
		case form.Has(name):
			out[name] = form.Get(name)
		}
	}
	return out
}

func (h *Handler) configSave(c *ctx) error {
	key := c.form.Get("key")
	if !slices.Contains(config.OverrideSections, key) {
		return fail(http.StatusBadRequest, "error.badForm")
	}
	page := h.configPage
	switch key {
	case "limits":
		page = h.limitsPage
	case "site":
		page = h.sitePage
	}
	o, err := config.FormOverride(key, formValues(c.form, key, nil))
	if err != nil {
		return c.retry(badConfig(err), page)
	}
	back := "/admin/config"
	if b := c.form.Get("back"); b != "" {
		back = safeNext(b)
	}
	return h.saveConfig(c, key, o.Value, back, page)
}

// configReset removes one override; back is the page it was reset from (the configuration page by default).
func (h *Handler) configReset(c *ctx) error {
	key := c.form.Get("key")
	back := "/admin/config"
	if b := c.form.Get("back"); b != "" {
		back = safeNext(b)
	}
	if err := h.ops.ResetConfig(c.actor(), key); err != nil {
		return c.retry(err, h.configPage)
	}
	return c.done(back, "config.reset", key)
}

// limitsSave saves the rate limits, edited on the limits page.
func (h *Handler) limitsSave(c *ctx) error {
	o, err := config.FormOverride("limits", formValues(c.form, "limits", nil))
	if err != nil {
		return c.retry(badConfig(err), h.limitsPage)
	}
	return h.saveConfig(c, "limits", o.Value, "/admin/limits", h.limitsPage)
}

// siteStatusSave saves whether the usage page shows the service status, edited on the usage page's page.
func (h *Handler) siteStatusSave(c *ctx) error {
	o, err := config.FormOverride("site", formValues(c.form, "site", nil))
	if err != nil {
		return c.retry(badConfig(err), h.sitePage)
	}
	return h.saveConfig(c, "site", o.Value, "/admin/site", h.sitePage)
}

// configExport downloads the console overrides in effect: format=overrides (only the changed parts) or
// merged (merged into the configuration file).
func (h *Handler) configExport(c *ctx) error {
	format := c.r.URL.Query().Get("format")
	b, err := h.ops.ExportConfig(format)
	if err != nil {
		return err
	}
	name := "config-overrides.yaml"
	if format == "merged" {
		name = "config.yaml"
	}
	hd := c.w.Header()
	hd.Set("Content-Type", "application/yaml; charset=utf-8")
	hd.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(b)
	return nil
}

// ---- A single upstream

type upstreamEditData struct {
	Editable    bool
	New         bool
	Name        string
	Source      string
	Credentials bool
	Fields      []cfgField
	Groups      []fieldGroup // Fields in the sections of the form
	Override    *ops.OverrideView
}

// fieldGroup is one section of a form: a title, what it is for, and its fields.
type fieldGroup struct {
	Key, Title, Help string
	Fields           []cfgField
}

// upstreamGroups are the sections of an upstream's form, by field.
var upstreamGroups = []struct {
	key    string
	fields []string
}{
	{"address", []string{"registry", "aliases", "library_prefix", "disabled"}},
	{"redirects", []string{"redirect_hosts", "extra_redirect_hosts", "unlisted_redirect", "token_realm_hosts"}},
}

func groupFields(lang i18n.Lang, fs []cfgField) []fieldGroup {
	var out []fieldGroup
	for _, g := range upstreamGroups {
		fg := fieldGroup{Key: g.key, Title: tr(lang, "upstream.group."+g.key), Help: tr(lang, "upstream.group."+g.key+"Help")}
		for _, f := range fs {
			if slices.Contains(g.fields, f.Name) {
				fg.Fields = append(fg.Fields, f)
			}
		}
		out = append(out, fg)
	}
	return out
}

func (h *Handler) upstreamEditPage(c *ctx) error {
	rt := h.app.Runtime()
	name := strings.ToLower(strings.TrimSpace(c.r.URL.Query().Get("name")))
	if c.form != nil && c.form.Has("upstream") {
		name = strings.ToLower(strings.TrimSpace(c.form.Get("upstream")))
	}
	d := upstreamEditData{Editable: h.canEditConfig(c), Name: name, Source: "config"}
	key := config.UpstreamKey(name)
	if config.OverrideFields(key) == nil {
		key = config.UpstreamKey("new.example.com") // name not filled in yet or invalid: only to list the fields
	}
	d.Override = overrideOf(h.ops.Overrides(), key)
	u := rt.Config.Upstreams[name]
	eff := rt.Config
	switch {
	case u == nil && d.Override != nil:
		// An upstream created in the console whose override has not taken effect: show what was stored;
		// it can be edited and saved again, or deleted
		d.Source = "console"
		if res, err := config.ParseWithOverrides(nil, []config.Override{{Key: key, Value: d.Override.Value}}); err == nil {
			eff = res.Config
		}
	case u == nil && name != "" && (c.form == nil || c.form.Get("new") == ""):
		return fail(http.StatusNotFound, "error.notFound")
	case u == nil:
		d.New = true
	case rt.FileConfig.Upstreams[name] == nil:
		d.Source = "console"
	case config.BuiltinUpstreams()[name] != nil:
		d.Source = "builtin"
	}
	if c.form != nil && c.form.Get("new") != "" {
		d.New = true
	}
	d.Fields = fields(c.lang, eff, rt.FileConfig, key, c.form)
	if d.New {
		d.Override = nil
		if c.form == nil {
			for i := range d.Fields {
				d.Fields[i].Value, d.Fields[i].Changed = "", false
			}
		}
	}
	d.Credentials = u != nil && u.Credentials != nil
	d.Groups = groupFields(c.lang, d.Fields)
	title := name
	if d.New {
		title = tr(c.lang, "config.upstreamNew")
	}
	return h.render(c, "upstream", &view{Title: title, Nav: "upstreams", Crumbs: []crumb{{tr(c.lang, "nav.upstreams"), "/admin/upstreams"}}, Data: d})
}

func (h *Handler) upstreamSave(c *ctx) error {
	rt := h.app.Runtime()
	name := strings.ToLower(strings.TrimSpace(c.form.Get("upstream")))
	key := config.UpstreamKey(name)
	if config.OverrideFields(key) == nil {
		return c.retry(ops.NewError(http.StatusBadRequest, "config.upstreamName"), h.upstreamEditPage)
	}
	isNew := c.form.Get("new") != ""
	if isNew && rt.Config.Upstreams[name] != nil {
		return c.retry(ops.NewError(http.StatusConflict, "config.upstreamExists", name), h.upstreamEditPage)
	}
	locked := func(f string) bool {
		u := rt.FileConfig.Upstreams[name]
		return u != nil && u.Credentials != nil && config.CredentialLocked(f)
	}
	values := formValues(c.form, key, locked)
	if isNew {
		// When creating, empty fields are not written, so the defaults apply
		for k, v := range values {
			if strings.TrimSpace(v) == "" || v == "false" {
				delete(values, k)
			}
		}
	}
	o, err := config.FormOverride(key, values)
	if err != nil {
		return c.retry(badConfig(err), h.upstreamEditPage)
	}
	return h.saveConfig(c, key, o.Value, "/admin/upstreams/edit?name="+url.QueryEscape(name), h.upstreamEditPage)
}
