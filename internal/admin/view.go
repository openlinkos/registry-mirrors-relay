package admin

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/status"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

//go:embed templates
var templateFS embed.FS

// view is the data handed to the templates: the page's own data is in Data, the rest is used by the
// layout.
type view struct {
	c *ctx
	h *Handler

	Title  string
	Nav    string // the page highlighted in the sidebar
	Crumbs []crumb
	Bare   bool // login, re-authentication: the form sits next to the brand panel, without the sidebar
	Data   any
	Notice string
	Body   template.HTML // the page body, rendered first and inserted by the layout
}

type crumb struct{ Label, Href string }

// T translates interface text. Arguments come in pairs, name and value, and replace {name} in the text.
func (v *view) T(key string, args ...any) string { return tr(v.c.lang, key, args...) }

func (v *view) Lang() string     { return v.c.lang }
func (v *view) HTMLLang() string { return htmlLang(v.c.lang) }
func (v *view) OtherLang() string {
	if v.c.lang == "zh" {
		return "en"
	}
	return "zh"
}
func (v *view) OtherHTMLLang() string { return htmlLang(v.OtherLang()) }
func (v *view) Theme() string         { return v.c.theme }
func (v *view) Sess() *session        { return v.c.sess }
func (v *view) CSRF() string {
	if v.c.sess == nil {
		return ""
	}
	return v.c.sess.CSRF
}
func (v *view) Asset(name string) string { return v.h.assets.url(name) }
func (v *view) Here() string             { return v.c.here() }
func (v *view) SiteName() string         { return v.h.app.Runtime().Site.Name }
func (v *view) Version() string          { return version.Version }
func (v *view) Writable() bool           { return v.h.app.Store.Writable() }
func (v *view) IsAdmin() bool            { return v.c.sess != nil && v.c.sess.Role == "admin" }

// CanWrite: the admin role, and the operational data is writable.
func (v *view) CanWrite() bool { return v.IsAdmin() && v.Writable() }

// CanEditConfig: the configuration can be changed in the console (admin role, operational data
// writable, and no admin.config_edit: false).
func (v *view) CanEditConfig() bool { return v.h.canEditConfig(v.c) }

// FormErr is the explanation shown when a form has an error.
func (v *view) FormErr() string { return v.c.formErr }

// Warns are the things to point out after a successful save (for example new warnings caused by a
// console override).
func (v *view) Warns() []string { return v.c.warnings }

// F is the value the user filled in, when a form is re-rendered after an error.
func (v *view) F(name string) string {
	if v.c.form == nil {
		return ""
	}
	return v.c.form.Get(name)
}

// Q is a query parameter.
func (v *view) Q(name string) string { return v.c.r.URL.Query().Get(name) }

type navItem struct {
	Key, Path, Label, Icon string
	Current                bool
}

type navGroup struct {
	Label string
	Items []navItem
}

// navTable is the sidebar: pages grouped by task.
var navTable = []struct {
	label string
	items [][3]string // key, path, icon
}{
	{"", [][3]string{{"overview", "/admin/", "layout-dashboard"}}},
	{"nav.groupService", [][3]string{{"upstreams", "/admin/upstreams", "server"}, {"usage", "/admin/usage", "plug"}}},
	{"nav.groupAccess", [][3]string{{"users", "/admin/users", "user"}, {"rules", "/admin/rules", "shield-check"}, {"limits", "/admin/limits", "activity"}}},
	{"nav.groupRecords", [][3]string{{"requests", "/admin/requests", "list"}, {"abuse", "/admin/abuse", "triangle-alert"}, {"audit", "/admin/audit", "scroll-text"}}},
	{"nav.groupSystem", [][3]string{{"settings", "/admin/settings", "settings"}, {"config", "/admin/config", "wrench"}, {"admins", "/admin/admins", "key-round"}, {"system", "/admin/system", "cpu"}}},
}

func (v *view) NavGroups() []navGroup {
	var out []navGroup
	for _, g := range navTable {
		ng := navGroup{}
		if g.label != "" {
			ng.Label = v.T(g.label)
		}
		for _, it := range g.items {
			ng.Items = append(ng.Items, navItem{Key: it[0], Path: it[1], Label: v.T("nav." + it[0]), Icon: it[2], Current: it[0] == v.Nav})
		}
		out = append(out, ng)
	}
	return out
}

// Group is the sidebar group of the current page, placed first in the breadcrumbs.
func (v *view) Group() string {
	for _, g := range navTable {
		for _, it := range g.items {
			if it[0] == v.Nav && g.label != "" {
				return v.T(g.label)
			}
		}
	}
	return ""
}

// ---- Template functions

var funcs = template.FuncMap{
	"icon":  icon,
	"bytes": fmtBytes,
	"num":   fmtNum,
	"pct":   fmtPct,
	"secs":  fmtSecs,
	"time":  timeTag,
	"dict":  dict,
	"join":  strings.Join,
	"add":   func(a, b int) int { return a + b },
	"tone":  statusTone,
	"str":   func(v any) string { return fmt.Sprint(v) },
	"list":  func(s ...string) []string { return s },
	// Strips covering the last 24 hours
	"hours":     hoursStrip,
	"ratios":    ratioStrip,
	"hoursPct":  hoursPct,
	"lastRatio": lastRatio,
	"levelTone": func(v float64) string {
		return map[string]string{"ok": "success", "mid": "warning", "bad": "danger"}[status.Level(v)]
	},
}

// parseTemplates puts the layout, the components and every page into one template set: the
// {{define "content"}} in each page file is renamed to "page/<file name>". html/template analyses the
// escaping of each template only once, and the shared layout and components are stored only once.
func parseTemplates() (*template.Template, error) {
	t, err := template.New("base").Funcs(funcs).ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if name == "base" {
			continue
		}
		b, err := templateFS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		text := strings.Replace(string(b), `{{define "content"}}`, `{{define "page/`+name+`"}}`, 1)
		if _, err := t.New(f).Parse(text); err != nil {
			return nil, fmt.Errorf("%s：%w", f, err)
		}
		if t.Lookup("page/"+name) == nil {
			return nil, fmt.Errorf("%s 里没有 {{define \"content\"}}", f)
		}
	}
	return t, nil
}

func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict 要成对的参数")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict 的键要是字符串")
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// fmtBytes uses binary units: 1.5 GiB.
func fmtBytes(v any) string {
	var n float64
	switch x := v.(type) {
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case uint64:
		n = float64(x)
	case float64:
		n = x
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatFloat(n, 'f', 0, 64) + " B"
	}
	prec := 1
	if n >= 100 {
		prec = 0
	}
	return strconv.FormatFloat(n, 'f', prec, 64) + " " + units[i]
}

// fmtNum adds thousands separators: 12,345. Fractions keep two decimal places.
func fmtNum(v any) string {
	var s string
	switch x := v.(type) {
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case uint64:
		s = strconv.FormatUint(x, 10)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			s = strconv.FormatInt(int64(x), 10)
		} else {
			return strconv.FormatFloat(x, 'f', 2, 64)
		}
	default:
		return fmt.Sprint(v)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtPct is part/total as a percentage, or — when total is 0.
func fmtPct(part, total any) string {
	p, t := toFloat(part), toFloat(total)
	if t == 0 {
		return "—"
	}
	return strconv.FormatFloat(p/t*100, 'f', 1, 64) + "%"
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// fmtSecs shows a duration: a number of seconds or a time.Duration.
func fmtSecs(v any) string {
	var d time.Duration
	switch x := v.(type) {
	case float64:
		d = time.Duration(x * float64(time.Second))
	case time.Duration:
		d = x
	}
	switch {
	case d < time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Microsecond), 'f', 0, 64) + " µs"
	case d < time.Second:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 0, 64) + " ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	}
	return d.Round(time.Second).String()
}

// timeTag is a <time>: the server writes UTC and the script converts it to the browser's time zone. The
// zero value is shown as —.
func timeTag(t time.Time) template.HTML {
	if t.IsZero() {
		return "—"
	}
	u := t.UTC()
	return template.HTML(`<time datetime="` + u.Format(time.RFC3339) + `">` + u.Format("2006-01-02 15:04") + ` UTC</time>`) //nolint:gosec // only the formatted time
}

// statusTone is the badge colour for an HTTP status code.
func statusTone(status int) string {
	switch {
	case status >= 500:
		return "danger"
	case status == 429 || status >= 400:
		return "warning"
	case status >= 300:
		return "info"
	}
	return "success"
}

// Dur is the written form of a duration.
func (v *view) Dur(d time.Duration) string { return fmtDur(v.c.lang, d) }

// Ago is how long ago t was.
func (v *view) Ago(t time.Time) string { return ago(v.c.lang, v.c.now, t) }

// Now is the time of this request.
func (v *view) Now() time.Time { return v.c.now }
