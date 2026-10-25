package i18n

import (
	"errors"
	"fmt"
	"testing"
)

func TestNegotiate(t *testing.T) {
	for _, c := range []struct {
		cookie, accept string
		want           Lang
	}{
		{"", "", EN},
		{"zh", "en", ZH},
		{"en", "zh-CN", EN},
		{"zh-CN", "", ZH},
		{"fr", "zh-CN,zh;q=0.9,en;q=0.8", ZH},
		{"", "en-GB,en;q=0.9,zh;q=0.8", EN},
		{"", "ja,zh-TW;q=0.7,en;q=0.5", ZH},
		{"", "ja,fr;q=0.5", EN},
		{"", "zh;q=0,en", EN},
		{"", "zh;q=0.3,en;q=0.7", EN},
		{"", "zh;q=bad,en", EN},
	} {
		if got := Negotiate(c.cookie, c.accept); got != c.want {
			t.Errorf("Negotiate(%q, %q) = %q, want %q", c.cookie, c.accept, got, c.want)
		}
	}
}

func withCatalogue(t *testing.T, m map[string]string) {
	t.Helper()
	old := zh
	zh = m
	t.Cleanup(func() { zh = old })
}

func TestRender(t *testing.T) {
	withCatalogue(t, map[string]string{
		"entrance %s":                "入口 %s",
		"%s: must be at least %d":    "%s：至少要 %d",
		"reload failed: %w":          "重新加载失败：%w",
		"%[1]s is used by %[2]s":     "%[2]s 用到了 %[1]s",
		"configuration errors (%d):": "配置有 %d 处错误：",
	})
	where := M("entrance %s", "default")
	err := Errorf("%s: must be at least %d", where, 2)
	if got := err.Error(); got != "entrance default: must be at least 2" {
		t.Errorf("Error() = %q", got)
	}
	if got := Localize(err, ZH); got != "入口 default：至少要 2" {
		t.Errorf("Localize zh = %q", got)
	}

	wrapped := Errorf("reload failed: %w", err)
	if !errors.Is(wrapped, err) {
		t.Error("%w does not wrap")
	}
	if got := Localize(wrapped, ZH); got != "重新加载失败：入口 default：至少要 2" {
		t.Errorf("wrapped zh = %q", got)
	}
	if got := T(ZH, "%[1]s is used by %[2]s", "a", "b"); got != "b 用到了 a" {
		t.Errorf("indexed = %q", got)
	}
	if got := T(ZH, "untranslated %d", 3); got != "untranslated 3" {
		t.Errorf("fallback = %q", got)
	}

	joined := errors.Join(err, fmt.Errorf("plain"))
	if got := Localize(joined, ZH); got != "入口 default：至少要 2\nplain" {
		t.Errorf("joined = %q", got)
	}
	if got := Localize(fmt.Errorf("outer: %w", err), ZH); got != "outer: entrance default: must be at least 2" {
		t.Errorf("fmt wrapper = %q", got)
	}
}

func TestCode(t *testing.T) {
	inner := Coded("admin.last", "at least one administrator must remain")
	outer := Errorf("not saved: %w", inner)
	if got := Code(outer); got != "admin.last" {
		t.Errorf("Code = %q", got)
	}
	if got := Code(fmt.Errorf("x: %w", errors.Join(errors.New("a"), outer))); got != "admin.last" {
		t.Errorf("Code through join = %q", got)
	}
	if Code(errors.New("plain")) != "" || Code(nil) != "" {
		t.Error("Code of a plain error is not empty")
	}
}

func TestMessage(t *testing.T) {
	var zero Message
	if !zero.IsZero() || zero.In(ZH) != "" {
		t.Error("zero message is not empty")
	}
	if got := M("100%% done").String(); got != "100% done" {
		t.Errorf("percent = %q", got)
	}
	if ZH.HTML() != "zh-CN" || EN.HTML() != "en" || EN.Name() != "English" {
		t.Error("HTML or Name")
	}
}
