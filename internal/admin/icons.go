package admin

import (
	"bytes"
	"html/template"
	"regexp"
	"slices"
	"strings"
)

// iconNames 是后台用到的 Lucide 图标，全部来自设计系统的图标集（assets/vendor/icons.json）；加载时检查每个都在。
var iconNames = []string{
	"activity", "arrow-left", "ban", "check", "circle-alert", "circle-check", "clock", "copy", "cpu", "database",
	"download", "external-link", "filter", "globe", "hard-drive", "info", "key-round", "languages", "layout-dashboard",
	"list", "lock", "log-in", "log-out", "menu", "moon", "pencil", "plug", "plus", "refresh-cw", "scroll-text",
	"search", "server", "settings", "shield-check", "sun", "sun-moon", "terminal", "trash-2", "triangle-alert",
	"user", "x",
}

var iconNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// iconPrefix 让 symbol 的 id 不会和页面里的其他 id 冲突。
const iconPrefix = "i-"

// icon 画一个图标，引用页面自带的 symbol：<svg class="ol-icon"><use href="#i-search"/></svg>。
// 页面渲染完后按引用到的名字生成 symbol（见 withSprite），所以每页只带自己用到的图标。
func icon(name string, size ...string) template.HTML {
	if !slices.Contains(iconNames, name) {
		panic("没有登记的图标 " + name) // 开发时的错误，测试会覆盖到每个页面
	}
	cls := "ol-icon"
	if len(size) > 0 && (size[0] == "sm" || size[0] == "lg") {
		cls += " ol-icon-" + size[0]
	}
	return template.HTML(`<svg class="` + cls + `" aria-hidden="true"><use href="#` + iconPrefix + name + `"/></svg>`) //nolint:gosec // 名字来自上面的白名单
}

// spriteMark 是版式里放图标 symbol 的位置，渲染完后替换掉。
const spriteMark = `<svg data-sprite hidden></svg>`

var iconRef = regexp.MustCompile(`<use href="#` + iconPrefix + `([a-z0-9-]+)"/>`)

// withSprite 把页面里引用到的图标的 symbol 放到 spriteMark 的位置。文本里的值都经过转义，
// 不会出现这种带引号的写法，所以只会匹配到 icon() 生成的标记。
func (a *assets) withSprite(page []byte) []byte {
	seen := map[string]bool{}
	for _, m := range iconRef.FindAllSubmatch(page, -1) {
		seen[string(m[1])] = true
	}
	var b strings.Builder
	if len(seen) > 0 {
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		slices.Sort(names)
		b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" hidden aria-hidden="true">`)
		for _, n := range names {
			b.WriteString(`<symbol id="` + iconPrefix + n + `" viewBox="` + template.HTMLEscapeString(a.viewBox) + `">`)
			b.WriteString(a.icons[n]) // 加载时已检查过只是基本图形
			b.WriteString(`</symbol>`)
		}
		b.WriteString(`</svg>`)
	}
	return bytes.Replace(page, []byte(spriteMark), []byte(b.String()), 1)
}
