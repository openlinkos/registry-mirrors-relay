package admin

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
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

type cfgUpstream struct {
	Name, Registry string
	Disabled       bool
	Credentials    bool
	Source         string // builtin | config | console
	Override       *ops.OverrideView
}

type configData struct {
	Editable  bool // can save: admin role, operational data writable, and no admin.config_edit: false
	EditOff   bool // admin.config_edit: false
	Overrides []ops.OverrideView
	Sections  []cfgSection
	Entrances []entranceView
	EntOver   *ops.OverrideView
	Upstreams []cfgUpstream
	Effective string // the effective configuration (YAML, secrets hidden)
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
	d := configData{Editable: h.canEditConfig(c), EditOff: !rt.FileConfig.Admin.ConfigEdit, Overrides: h.ops.Overrides()}
	for _, key := range config.OverrideSections {
		var form url.Values
		if c.form != nil && c.form.Get("key") == key {
			form = c.form
		}
		d.Sections = append(d.Sections, cfgSection{Key: key, Title: tr(c.lang, "cfg."+key), Help: tr(c.lang, "cfg."+key+"Help"),
			Fields: fields(c.lang, rt.Config, rt.FileConfig, key, form), Override: overrideOf(d.Overrides, key)})
	}
	for _, e := range rt.Config.Entrances {
		d.Entrances = append(d.Entrances, entranceView{Name: e.Name, Hosts: strings.Join(e.Hosts, ", "), Upstream: e.Upstream, Auth: e.Auth, Page: e.Page})
	}
	d.EntOver = overrideOf(d.Overrides, "entrances")
	builtin := config.BuiltinUpstreams()
	names := make([]string, 0, len(rt.Config.Upstreams))
	for n := range rt.Config.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := rt.Config.Upstreams[n]
		cu := cfgUpstream{Name: n, Registry: u.Registry, Disabled: u.Disabled, Credentials: u.Credentials != nil, Source: "config",
			Override: overrideOf(d.Overrides, config.UpstreamKey(n))}
		switch {
		case rt.FileConfig.Upstreams[n] == nil:
			cu.Source = "console"
		case builtin[n] != nil:
			cu.Source = "builtin"
		}
		d.Upstreams = append(d.Upstreams, cu)
	}
	// Upstreams created in the console whose override has not taken effect (not in the effective
	// configuration)
	for i := range d.Overrides {
		o := &d.Overrides[i]
		if name, ok := strings.CutPrefix(o.Key, "upstreams/"); ok && rt.Config.Upstreams[name] == nil {
			d.Upstreams = append(d.Upstreams, cfgUpstream{Name: name, Registry: "—", Source: "console", Override: o})
		}
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
	o, err := config.FormOverride(key, formValues(c.form, key, nil))
	if err != nil {
		return c.retry(badConfig(err), h.configPage)
	}
	return h.saveConfig(c, key, o.Value, "/admin/config", h.configPage)
}

func (h *Handler) configReset(c *ctx) error {
	key := c.form.Get("key")
	if err := h.ops.ResetConfig(c.actor(), key); err != nil {
		return c.retry(err, h.configPage)
	}
	return c.done("/admin/config", "config.reset", key)
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

// ---- Entrance list

// entranceRow is one row in the entrance form.
type entranceRow struct {
	Index                                              int
	Name, Hosts, Upstream, DefaultUpstream, Auth, Page string
}

type entrancesData struct {
	Editable  bool
	Rows      []entranceRow
	Upstreams []string // the upstreams that can be chosen (those not disabled)
	Override  *ops.OverrideView
	FileHosts []string // the entrances in the configuration file, for comparison
}

// blankEntranceRows is the number of empty rows left at the end of the form; filling one in makes a new
// entrance.
const blankEntranceRows = 2

const maxEntranceRows = 200

func (h *Handler) entrancesPage(c *ctx) error {
	rt := h.app.Runtime()
	d := entrancesData{Editable: h.canEditConfig(c), Override: overrideOf(h.ops.Overrides(), "entrances")}
	for n, u := range rt.Config.Upstreams {
		if !u.Disabled {
			d.Upstreams = append(d.Upstreams, n)
		}
	}
	sort.Strings(d.Upstreams)
	if f := c.form; f != nil && f.Has("rows") {
		n, _ := strconv.Atoi(f.Get("rows"))
		for i := range min(n, maxEntranceRows) {
			p := strconv.Itoa(i)
			if f.Get("delete_"+p) != "" {
				continue
			}
			d.Rows = append(d.Rows, entranceRow{Name: f.Get("name_" + p), Hosts: f.Get("hosts_" + p), Upstream: f.Get("upstream_" + p),
				DefaultUpstream: f.Get("default_upstream_" + p), Auth: f.Get("auth_" + p), Page: f.Get("page_" + p)})
		}
	} else {
		for _, e := range rt.Config.Entrances {
			d.Rows = append(d.Rows, entranceRow{Name: e.Name, Hosts: strings.Join(e.Hosts, ", "), Upstream: e.Upstream,
				DefaultUpstream: e.DefaultUpstream, Auth: e.Auth, Page: e.Page})
		}
	}
	if d.Editable {
		for range blankEntranceRows {
			d.Rows = append(d.Rows, entranceRow{Auth: "none", Page: "public"})
		}
	}
	for i := range d.Rows {
		d.Rows[i].Index = i
	}
	for _, e := range rt.FileConfig.Entrances {
		d.FileHosts = append(d.FileHosts, e.Name+": "+strings.Join(e.Hosts, ", "))
	}
	return h.render(c, "entrances", &view{Title: tr(c.lang, "config.entrancesTitle"), Nav: "config",
		Crumbs: []crumb{{tr(c.lang, "nav.config"), "/admin/config"}}, Data: d})
}

func (h *Handler) entrancesSave(c *ctx) error {
	n, err := strconv.Atoi(c.form.Get("rows"))
	if err != nil || n < 0 || n > maxEntranceRows {
		return fail(http.StatusBadRequest, "error.badForm")
	}
	var list []map[string]string
	for i := range n {
		p := strconv.Itoa(i)
		if c.form.Get("delete_"+p) != "" || strings.TrimSpace(c.form.Get("hosts_"+p)) == "" {
			continue
		}
		row := map[string]string{}
		for _, f := range config.OverrideFields("entrances") {
			row[f] = c.form.Get(f + "_" + p)
		}
		list = append(list, row)
	}
	o, err := config.FormEntrances(list)
	if err != nil {
		return c.retry(badConfig(err), h.entrancesPage)
	}
	return h.saveConfig(c, "entrances", o.Value, "/admin/config/entrances", h.entrancesPage)
}

// ---- A single upstream

type upstreamEditData struct {
	Editable    bool
	New         bool
	Name        string
	Source      string
	Credentials bool
	Fields      []cfgField
	Override    *ops.OverrideView
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
	title := name
	if d.New {
		title = tr(c.lang, "config.upstreamNew")
	}
	return h.render(c, "upstream", &view{Title: title, Nav: "config", Crumbs: []crumb{{tr(c.lang, "nav.config"), "/admin/config#upstreams"}}, Data: d})
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
	return h.saveConfig(c, key, o.Value, "/admin/config/upstream?name="+url.QueryEscape(name), h.upstreamEditPage)
}
