package ui

import (
	"regexp"
	"strings"
	"testing"
)

func TestSet(t *testing.T) {
	s, err := NewSet()
	if err != nil {
		t.Fatal(err)
	}
	css, ok := s.Lookup(s.File("ui.css"))
	if !ok || css.Type != CSS {
		t.Fatal("no ui.css")
	}
	// Fonts are served next to the stylesheet under their hashed names
	fonts := regexp.MustCompile(`url\(([^)]+\.woff2)\)`).FindAllStringSubmatch(string(css.Body), -1)
	if len(fonts) == 0 {
		t.Fatal("the stylesheet refers to no fonts")
	}
	for _, m := range fonts {
		f, ok := s.Lookup(m[1])
		if !ok || f.Type != "font/woff2" || string(f.Body[:4]) != "wOF2" {
			t.Errorf("font %s is not served", m[1])
		}
	}
	// Every brand and the neutral look have their marks
	for _, b := range append([]string{""}, brandIDs()...) {
		for _, f := range MarkFiles {
			a, ok := s.Lookup(s.File(Mark(b, f)))
			if !ok || a.Type != SVG || !strings.HasPrefix(string(a.Body), "<svg") {
				t.Errorf("brand %q has no %s", b, f)
			}
		}
	}
	if s.File(Mark("linkos", "logo.svg")) == s.File(Mark("origin", "logo.svg")) {
		t.Error("marks of different brands must not share a file name")
	}
	a := s.Add("admin.js", JS, []byte("x"))
	if got, ok := s.Lookup(a.File); !ok || got != a || !regexp.MustCompile(`^admin\.[0-9a-f]{10}\.js$`).MatchString(a.File) {
		t.Errorf("Add: %+v", a)
	}
}

func TestBrands(t *testing.T) {
	ids := brandIDs()
	for _, want := range []string{"linkos", "linkos-mirrors", "openlinkos", "origin"} {
		if !IsBrand(want) {
			t.Errorf("missing brand %s (have %v)", want, ids)
		}
	}
	if IsBrand("") || IsBrand("nope") {
		t.Error("IsBrand accepts what is not a brand")
	}
	if BrandName("linkos-mirrors") != "LinkOS Mirrors" {
		t.Errorf("BrandName = %q", BrandName("linkos-mirrors"))
	}
}

func TestIcons(t *testing.T) {
	page := []byte(`<body>` + SpriteMark + string(Icon("search")) + string(Icon("copy", "sm")) + string(Icon("search")) + `</body>`)
	out := string(WithSprite(page))
	if strings.Count(out, "<symbol ") != 2 || !strings.Contains(out, `id="i-copy"`) || !strings.Contains(out, `class="ol-icon ol-icon-sm"`) {
		t.Errorf("sprite: %s", out)
	}
	if strings.Contains(out, SpriteMark) {
		t.Error("the sprite mark stays in the page")
	}
	defer func() {
		if recover() == nil {
			t.Error("an unknown icon should panic")
		}
	}()
	Icon("no-such-icon")
}

func TestVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version()) {
		t.Errorf("Version() = %q", Version())
	}
}
