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

// The design system (@openlinkos/ui, placed in assets/vendor by scripts/vendor-ui.sh) and the console's
// own static files.
//
//go:embed assets
var assetFS embed.FS

// asset is a static file, served under a name that carries its content hash: /admin/assets/<file>, so it
// can be cached for a long time.
type asset struct {
	name  string // logical name, e.g. admin.css
	file  string // hashed name, e.g. admin.1a2b3c4d5e.css
	ctype string
	body  []byte
}

type assets struct {
	byName  map[string]*asset
	byFile  map[string]*asset
	icons   map[string]string
	viewBox string
}

// fontURL matches url(fonts/<name>.woff2) in the design system's stylesheet.
var fontURL = regexp.MustCompile(`url\((["']?)fonts/([A-Za-z0-9_-]+)\.woff2(["']?)\)`)

// safeIcon is the content of an icon symbol: only basic shapes with geometry and fill attributes are
// allowed. Icons are inlined into pages as markup, so anything else (scripts, event attributes, links,
// styles) is rejected at load time.
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
		return nil, fmt.Errorf("icons.json: %w", err)
	}
	a.viewBox = icons.ViewBox
	for name, body := range icons.Icons {
		if !iconNameRe.MatchString(name) || !safeIcon.MatchString(body) {
			return nil, fmt.Errorf("@openlinkos/ui icon %q is not plain shapes", name)
		}
		a.icons[name] = body
	}
	for _, name := range iconNames {
		if _, ok := a.icons[name]; !ok {
			return nil, fmt.Errorf("@openlinkos/ui has no icon %q", name)
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

// withFonts serves the fonts the stylesheet references under hashed names as well
// (/admin/assets/fonts/…) and rewrites their URLs in the stylesheet.
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

// url returns the URL of a static file.
func (a *assets) url(name string) string {
	if f, ok := a.byName[name]; ok {
		return "/admin/assets/" + f.file
	}
	return ""
}
