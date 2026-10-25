// Package ui is the design system the admin console and the usage page share: @openlinkos/ui (the
// stylesheet, its fonts and the icons) and the brands' marks from @openlinkos/vitepress-theme, put into
// vendor/ by scripts/vendor-ui.sh and embedded into the program. A site without a brand uses the neutral
// marks in neutral/.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed vendor neutral
var files embed.FS

// Content types of the files in a set.
const (
	CSS = "text/css; charset=utf-8"
	JS  = "text/javascript; charset=utf-8"
	SVG = "image/svg+xml"
)

// Asset is a static file, served under a name that carries its content hash (ui.1a2b3c4d5e.css), so it
// can be cached for a long time.
type Asset struct {
	Name string // logical name, e.g. ui.css or brands/linkos/logo.svg
	File string // hashed name, e.g. ui.1a2b3c4d5e.css
	Type string
	Body []byte
}

// Set is the static files of one site (the console, the usage page): the design system's stylesheet and
// fonts, every brand's marks, and the site's own files. Files are served under one path prefix, fonts
// next to the stylesheet.
type Set struct {
	byName map[string]*Asset
	byFile map[string]*Asset
}

// fontURL matches url(fonts/<name>.woff2) in the design system's stylesheet.
var fontURL = regexp.MustCompile(`url\((["']?)fonts/([A-Za-z0-9_-]+)\.woff2(["']?)\)`)

// MarkFiles are the files of a brand's mark: the favicon (light and dark in one), and the logo for light
// and for dark backgrounds.
var MarkFiles = []string{"favicon.svg", "logo.svg", "logo-dark.svg"}

// NewSet returns a set with the design system's stylesheet, the fonts it refers to, and the marks of
// every brand and of the neutral look.
func NewSet() (*Set, error) {
	s := &Set{byName: map[string]*Asset{}, byFile: map[string]*Asset{}}
	css, err := files.ReadFile("vendor/ui.css")
	if err != nil {
		return nil, err
	}
	var ferr error
	css = fontURL.ReplaceAllFunc(css, func(m []byte) []byte {
		name := "fonts/" + string(fontURL.FindSubmatch(m)[2]) + ".woff2"
		f, ok := s.byName[name]
		if !ok {
			body, err := files.ReadFile("vendor/" + name)
			if err != nil {
				ferr = err
				return m
			}
			f = s.Add(name, "font/woff2", body)
		}
		return []byte("url(" + f.File + ")")
	})
	if ferr != nil {
		return nil, ferr
	}
	s.Add("ui.css", CSS, css)
	for _, b := range append([]string{""}, brandIDs()...) {
		for _, f := range MarkFiles {
			body, err := files.ReadFile(markPath(b, f))
			if err != nil {
				return nil, err
			}
			s.Add(Mark(b, f), SVG, body)
		}
	}
	return s, nil
}

// Add puts a file into the set under its hashed name.
func (s *Set) Add(name, ctype string, body []byte) *Asset {
	sum := sha256.Sum256(body)
	ext := path.Ext(name)
	file := strings.TrimSuffix(path.Base(name), ext) + "." + hex.EncodeToString(sum[:])[:10] + ext
	switch dir := path.Dir(name); dir {
	case ".":
	case "fonts":
		file = "fonts/" + file // the stylesheet refers to fonts/…
	default:
		file = strings.ReplaceAll(dir, "/", "-") + "-" + file // marks of different brands share their base names
	}
	a := &Asset{Name: name, File: file, Type: ctype, Body: body}
	s.byName[name] = a
	s.byFile[file] = a
	return a
}

// Lookup finds a file by its hashed name.
func (s *Set) Lookup(file string) (*Asset, bool) {
	a, ok := s.byFile[file]
	return a, ok
}

// File is the hashed name of a file, "" when the set has none of that name.
func (s *Set) File(name string) string {
	if a, ok := s.byName[name]; ok {
		return a.File
	}
	return ""
}

// ---- Brands

// Brand is one brand of the design system.
type Brand struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var brands = sync.OnceValue(func() []Brand {
	raw, err := files.ReadFile("vendor/brands/brands.json")
	if err != nil {
		panic(err) // embedded at build time; the tests load it
	}
	var all map[string]struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		panic(fmt.Sprintf("brands.json: %v", err))
	}
	css, _ := files.ReadFile("vendor/ui.css")
	var out []Brand
	for id, b := range all {
		// A brand needs its colours in the stylesheet and its marks
		if !bytes.Contains(css, []byte(`[data-brand="`+id+`"]`)) {
			continue
		}
		if _, err := files.ReadFile(markPath(id, "logo.svg")); err != nil {
			continue
		}
		out = append(out, Brand{ID: id, Name: b.Name})
	}
	slices.SortFunc(out, func(a, b Brand) int { return strings.Compare(a.ID, b.ID) })
	return out
})

// Brands lists the brands a site can take (site.brand): those with colours in the stylesheet and marks.
func Brands() []Brand { return slices.Clone(brands()) }

func brandIDs() []string {
	var ids []string
	for _, b := range brands() {
		ids = append(ids, b.ID)
	}
	return ids
}

// IsBrand reports whether id is one of Brands.
func IsBrand(id string) bool { return slices.Contains(brandIDs(), id) }

// BrandName is a brand's display name, "" for none.
func BrandName(id string) string {
	for _, b := range brands() {
		if b.ID == id {
			return b.Name
		}
	}
	return ""
}

// Mark is the name in a set of one file of a brand's mark (one of MarkFiles); an empty brand is the
// neutral look.
func Mark(brand, file string) string {
	if brand == "" {
		return "brands/neutral/" + file
	}
	return "brands/" + brand + "/" + file
}

func markPath(brand, file string) string {
	if brand == "" {
		return "neutral/" + file
	}
	return "vendor/brands/" + brand + "/" + file
}

// ---- Icons

// iconSet is the design system's icons (icons.json): only basic shapes are allowed, since icons are
// inlined into pages as markup.
var iconSet = sync.OnceValues(func() (map[string]string, string) {
	raw, err := files.ReadFile("vendor/icons.json")
	if err != nil {
		panic(err)
	}
	var icons struct {
		ViewBox string            `json:"viewBox"`
		Icons   map[string]string `json:"icons"`
	}
	if err := json.Unmarshal(raw, &icons); err != nil {
		panic(fmt.Sprintf("icons.json: %v", err))
	}
	for name, body := range icons.Icons {
		if !iconNameRe.MatchString(name) || !safeIcon.MatchString(body) {
			panic(fmt.Sprintf("@openlinkos/ui icon %q is not plain shapes", name))
		}
	}
	return icons.Icons, icons.ViewBox
})

var iconNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// safeIcon is the content of an icon symbol: basic shapes with geometry and fill attributes only.
// Anything else (scripts, event attributes, links, styles) is rejected.
var safeIcon = regexp.MustCompile(`^(?:\s*<(?:path|circle|rect|line|polyline|polygon|ellipse)(?:\s+(?:d|cx|cy|r|rx|ry|x|y|x1|x2|y1|y2|width|height|points|fill|stroke|stroke-width|stroke-linecap|stroke-linejoin)="[^"<>&]*")*\s*/>)*\s*$`)

// HasIcon reports whether the design system has an icon of this name.
func HasIcon(name string) bool {
	icons, _ := iconSet()
	_, ok := icons[name]
	return ok
}

// iconPrefix keeps symbol ids from clashing with other ids on a page.
const iconPrefix = "i-"

// Icon draws an icon that refers to a symbol the page carries: <svg class="ol-icon"><use
// href="#i-search"/></svg>. Size is "sm" or "lg"; the default is the medium size. Once a page is rendered,
// WithSprite adds the symbols it refers to, so each page carries only the icons it uses.
func Icon(name string, size ...string) template.HTML {
	if !HasIcon(name) {
		panic("@openlinkos/ui has no icon " + name) // a development error; the tests render every page
	}
	cls := "ol-icon"
	if len(size) > 0 && (size[0] == "sm" || size[0] == "lg") {
		cls += " ol-icon-" + size[0]
	}
	return template.HTML(`<svg class="` + cls + `" aria-hidden="true"><use href="#` + iconPrefix + name + `"/></svg>`) //nolint:gosec // the name is one of the design system's icons
}

// SpriteMark is the place in a page for the icon symbols; WithSprite replaces it.
const SpriteMark = `<svg data-sprite hidden></svg>`

var iconRef = regexp.MustCompile(`<use href="#` + iconPrefix + `([a-z0-9-]+)"/>`)

// WithSprite puts the symbols of the icons a page refers to in place of SpriteMark. Values in the text
// are escaped and never appear in this quoted form, so only the markup Icon generates matches.
func WithSprite(page []byte) []byte {
	icons, viewBox := iconSet()
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
			b.WriteString(`<symbol id="` + iconPrefix + n + `" viewBox="` + template.HTMLEscapeString(viewBox) + `">`)
			b.WriteString(icons[n]) // checked when loaded to be basic shapes only
			b.WriteString(`</symbol>`)
		}
		b.WriteString(`</svg>`)
	}
	return bytes.Replace(page, []byte(SpriteMark), []byte(b.String()), 1)
}

// Version is the design system's version, from vendor/VERSION.
func Version() string {
	b, _ := files.ReadFile("vendor/VERSION")
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimPrefix(line, "@openlinkos/ui ")
}
