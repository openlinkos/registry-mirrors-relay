package admin

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// ---- 配置：在后台改的配置存在运营数据库里，叠加在配置文件之上（见 config.Override）

// cfgField 是表单里的一个字段。
type cfgField struct {
	Name, Label, Hint string
	Kind              string   // bool int float duration size list string
	Value             string   // 生效的值；表单出错重新显示时是填过的
	File              string   // 配置文件里的值
	Changed           bool     // 生效的值和配置文件的不同（后台改过）
	Locked            bool     // 配了账号的上游，只能改配置文件
	Choices           []string // 只能从这些里选
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
	Editable  bool // 能保存：admin 角色、运营数据可写、没有写 admin.config_edit: false
	EditOff   bool // admin.config_edit: false
	Overrides []ops.OverrideView
	Sections  []cfgSection
	Entrances []entranceView
	EntOver   *ops.OverrideView
	Upstreams []cfgUpstream
	Effective string // 生效的配置（YAML，密钥隐去）
}

// fieldChoices 是只能从几个值里选的字段。
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

// fields 列出 key 这一段能改的字段：eff 是生效的配置，file 是配置文件的。form 不为 nil 时（表单出错重新显示）用填过的值。
func fields(lang string, eff, file *config.Config, key string, form url.Values) []cfgField {
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
		f.Changed = inFile && f.Value != f.File // 后台新建的上游没有配置文件里的值可比
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
	// 后台新建、但修改没有生效的上游（不在生效的配置里）
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

// saveConfig 保存一处修改。成功后跳回 back；有新的提醒时直接显示 page，把提醒列出来。
func (h *Handler) saveConfig(c *ctx, key, value, back string, page func(*ctx) error) error {
	warnings, err := h.ops.SetConfig(c.actor(), key, value)
	if err != nil {
		return c.retry(err, page)
	}
	if len(warnings) == 0 {
		return c.done(back, "config.saved", key)
	}
	c.form, c.warnings, c.flash = nil, warnings, tr(c.lang, "notice.config.saved", "v", key)
	return page(c)
}

func badConfig(err error) error {
	return &ops.Error{Status: http.StatusBadRequest, Key: "config.invalid", Detail: err.Error()}
}

// formValues 取表单里 key 这一段的字段。开关没勾选时浏览器不发，按 false。
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

// configExport 下载生效中的后台修改：format=overrides（只有改过的部分）或 merged（合进配置文件）。
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

// ---- 入口列表

// entranceRow 是入口表单里的一行。
type entranceRow struct {
	Index                                              int
	Name, Hosts, Upstream, DefaultUpstream, Auth, Page string
}

type entrancesData struct {
	Editable  bool
	Rows      []entranceRow
	Upstreams []string // 可以选的上游（没停用的）
	Override  *ops.OverrideView
	FileHosts []string // 配置文件里的入口，对照用
}

// blankEntranceRows 是表单末尾留的空行，填了就是新入口。
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
		d.FileHosts = append(d.FileHosts, e.Name+"："+strings.Join(e.Hosts, ", "))
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

// ---- 一个上游

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
		key = config.UpstreamKey("new.example.com") // 名字还没填或写错了：只为列出字段
	}
	d.Override = overrideOf(h.ops.Overrides(), key)
	u := rt.Config.Upstreams[name]
	eff := rt.Config
	switch {
	case u == nil && d.Override != nil:
		// 后台新建的上游，修改没有生效：按存下的内容显示，可以改了重新保存或者删除
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
		return c.retry(&ops.Error{Status: http.StatusBadRequest, Key: "config.upstreamName"}, h.upstreamEditPage)
	}
	isNew := c.form.Get("new") != ""
	if isNew && rt.Config.Upstreams[name] != nil {
		return c.retry(&ops.Error{Status: http.StatusConflict, Key: "config.upstreamExists", Arg: name}, h.upstreamEditPage)
	}
	locked := func(f string) bool {
		u := rt.FileConfig.Upstreams[name]
		return u != nil && u.Credentials != nil && config.CredentialLocked(f)
	}
	values := formValues(c.form, key, locked)
	if isNew {
		// 新建时空着的字段不写，用默认值
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
