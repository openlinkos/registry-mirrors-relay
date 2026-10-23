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

	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

//go:embed templates
var templateFS embed.FS

// view 是交给模板的数据：页面自己的数据在 Data，其余是版式要用的。
type view struct {
	c *ctx
	h *Handler

	Title  string
	Nav    string // 侧栏里高亮的页面
	Crumbs []crumb
	Bare   bool // 登录、重新输入口令：表单在品牌面板旁边，没有侧栏
	Data   any
	Notice string
}

type crumb struct{ Label, Href string }

// T 翻译界面文案。参数成对给出：名字、值，替换文案里的 {名字}。
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

// CanWrite：admin 角色，并且运营数据可写。
func (v *view) CanWrite() bool { return v.IsAdmin() && v.Writable() }

// FormErr 是表单出错时的说明。
func (v *view) FormErr() string { return v.c.formErr }

// F 是表单出错重新渲染时用户填过的值。
func (v *view) F(name string) string {
	if v.c.form == nil {
		return ""
	}
	return v.c.form.Get(name)
}

// Q 是查询参数。
func (v *view) Q(name string) string { return v.c.r.URL.Query().Get(name) }

type navItem struct {
	Key, Path, Label, Icon string
	Current                bool
}

type navGroup struct {
	Label string
	Items []navItem
}

// navTable 是侧栏：按任务分组的页面。
var navTable = []struct {
	label string
	items [][3]string // 键、路径、图标
}{
	{"", [][3]string{{"overview", "/admin/", "layout-dashboard"}}},
	{"nav.groupService", [][3]string{{"upstreams", "/admin/upstreams", "server"}, {"usage", "/admin/usage", "plug"}}},
	{"nav.groupAccess", [][3]string{{"users", "/admin/users", "user"}, {"rules", "/admin/rules", "shield-check"}, {"limits", "/admin/limits", "activity"}}},
	{"nav.groupRecords", [][3]string{{"requests", "/admin/requests", "list"}, {"abuse", "/admin/abuse", "triangle-alert"}, {"audit", "/admin/audit", "scroll-text"}}},
	{"nav.groupSystem", [][3]string{{"settings", "/admin/settings", "settings"}, {"admins", "/admin/admins", "key-round"}, {"system", "/admin/system", "cpu"}}},
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

// Group 是当前页面所在的侧栏分组，放在面包屑最前面。
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

// ---- 模板函数

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
}

func parseTemplates() (map[string]*template.Template, error) {
	base, err := template.New("base").Funcs(funcs).ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if name == "base" {
			continue
		}
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(templateFS, f); err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
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

// fmtBytes 用二进制单位：1.5 GiB。
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

// fmtNum 加千位分隔：12,345。小数保留两位。
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

// fmtPct 是 part/total 的百分比，total 为 0 时是 —。
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

// fmtSecs 显示耗时：秒数或 time.Duration。
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

// timeTag 是一个 <time>：服务端写 UTC，脚本改成浏览器的时区。零值显示为 —。
func timeTag(t time.Time) template.HTML {
	if t.IsZero() {
		return "—"
	}
	u := t.UTC()
	return template.HTML(`<time datetime="` + u.Format(time.RFC3339) + `">` + u.Format("2006-01-02 15:04") + ` UTC</time>`) //nolint:gosec // 只有格式化好的时间
}

// statusTone 是 HTTP 状态码对应的徽章颜色。
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

// Dur 是一段时长的写法。
func (v *view) Dur(d time.Duration) string { return fmtDur(v.c.lang, d) }

// Ago 是现在距 t 多久。
func (v *view) Ago(t time.Time) string { return ago(v.c.lang, v.c.now, t) }

// Now 是这次请求的时间。
func (v *view) Now() time.Time { return v.c.now }
