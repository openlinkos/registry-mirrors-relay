package admin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// 设计系统（@openlinkos/ui，由 scripts/vendor-ui.sh 放进 assets/vendor）和后台自己的静态文件。
//
//go:embed assets
var assetFS embed.FS

// asset 是一个静态文件，以带内容哈希的名字提供：/admin/assets/<file>，可以长期缓存。
type asset struct {
	name  string // 逻辑名，如 admin.css
	file  string // 带哈希的名字，如 admin.1a2b3c4d5e.css
	ctype string
	body  []byte
}

type assets struct {
	byName  map[string]*asset
	byFile  map[string]*asset
	icons   map[string]string
	viewBox string
}

// fontURL 匹配设计系统样式表里的 url(fonts/<名字>.woff2)。
var fontURL = regexp.MustCompile(`url\((["']?)fonts/([A-Za-z0-9_-]+)\.woff2(["']?)\)`)

// safeIcon 是图标 symbol 的内容：只允许基本图形和几何、填充属性。图标会以标记的形式内联进页面，
// 所以加载时就拒绝其他任何东西（脚本、事件属性、链接、样式）。
var safeIcon = regexp.MustCompile(`^(?:\s*<(?:path|circle|rect|line|polyline|polygon|ellipse)(?:\s+(?:d|cx|cy|r|rx|ry|x|y|x1|x2|y1|y2|width|height|points|fill|stroke|stroke-width|stroke-linecap|stroke-linejoin)="[^"<>&]*")*\s*/>)*\s*$`)

const cssType = "text/css; charset=utf-8"

func loadAssets() (*assets, error) {
	a := &assets{byName: map[string]*asset{}, byFile: map[string]*asset{}, icons: map[string]string{}}
	for _, src := range []struct{ name, path, ctype string }{
		{"ui.css", "assets/vendor/ui.css", cssType},
		{"admin.css", "assets/admin.css", cssType},
		{"admin.js", "assets/admin.js", "text/javascript; charset=utf-8"},
		{"favicon.svg", "assets/favicon.svg", "image/svg+xml"},
		{"logo.svg", "assets/logo.svg", "image/svg+xml"},
		{"logo-dark.svg", "assets/logo-dark.svg", "image/svg+xml"},
	} {
		body, err := assetFS.ReadFile(src.path)
		if err != nil {
			return nil, err
		}
		if src.name == "ui.css" {
			if body, err = a.withFonts(body); err != nil {
				return nil, err
			}
		}
		a.add(src.name, src.ctype, body)
	}

	raw, err := assetFS.ReadFile("assets/vendor/icons.json")
	if err != nil {
		return nil, err
	}
	var icons struct {
		ViewBox string            `json:"viewBox"`
		Icons   map[string]string `json:"icons"`
	}
	if err := json.Unmarshal(raw, &icons); err != nil {
		return nil, fmt.Errorf("icons.json：%w", err)
	}
	a.viewBox = icons.ViewBox
	for name, body := range icons.Icons {
		if !iconNameRe.MatchString(name) || !safeIcon.MatchString(body) {
			return nil, fmt.Errorf("@openlinkos/ui 的图标 %q 不是纯图形", name)
		}
		a.icons[name] = body
	}
	for _, name := range iconNames {
		if _, ok := a.icons[name]; !ok {
			return nil, fmt.Errorf("@openlinkos/ui 里没有图标 %q", name)
		}
	}
	return a, nil
}

func (a *assets) add(name, ctype string, body []byte) *asset {
	sum := sha256.Sum256(body)
	ext := path.Ext(name)
	f := &asset{name: name, file: strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:])[:10] + ext, ctype: ctype, body: body}
	a.byName[name] = f
	a.byFile[f.file] = f
	return f
}

// withFonts 让样式表引用的字体也以带哈希的名字提供（/admin/assets/fonts/…），并改写样式表里的地址。
func (a *assets) withFonts(css []byte) ([]byte, error) {
	var err error
	out := fontURL.ReplaceAllFunc(css, func(m []byte) []byte {
		font := string(fontURL.FindSubmatch(m)[2])
		name := "fonts/" + font + ".woff2"
		f, ok := a.byName[name]
		if !ok {
			body, rerr := assetFS.ReadFile("assets/vendor/" + name)
			if rerr != nil {
				err = rerr
				return m
			}
			f = a.add(name, "font/woff2", body)
		}
		return []byte("url(" + f.file + ")")
	})
	return out, err
}

// url 返回静态文件的地址。
func (a *assets) url(name string) string {
	if f, ok := a.byName[name]; ok {
		return "/admin/assets/" + f.file
	}
	return ""
}
