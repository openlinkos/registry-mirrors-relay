// Package i18n localises what people read: console pages, API errors and configuration problems.
//
// Code is written in English. A message is an English format string with its arguments, and the
// Chinese catalogue (zh.go) is keyed by that format string, gettext style, so a message without a
// translation falls back to English. Logs, CLI output and errors sent to registry clients stay in
// English and do not go through this package.
package i18n

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Lang is an interface language.
type Lang string

const (
	EN Lang = "en"
	ZH Lang = "zh"
)

// Langs lists the supported languages in menu order.
var Langs = []Lang{ZH, EN}

// Parse accepts a language code such as "en", "zh" or "zh-CN".
func Parse(s string) (Lang, bool) {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "-")
	switch base {
	case "en":
		return EN, true
	case "zh":
		return ZH, true
	}
	return "", false
}

// HTML returns the value for the lang attribute of a page.
func (l Lang) HTML() string {
	if l == ZH {
		return "zh-CN"
	}
	return "en"
}

// Name returns the language's name written in that language, as language menus show it.
func (l Lang) Name() string {
	if l == ZH {
		return zhName
	}
	return "English"
}

// Negotiate picks the interface language: an explicit choice (the cookie) first, then the browser's
// Accept-Language by quality, then English.
func Negotiate(cookie, acceptLanguage string) Lang {
	if l, ok := Parse(cookie); ok {
		return l
	}
	type pref struct {
		lang Lang
		q    float64
	}
	var prefs []pref
	for part := range strings.SplitSeq(acceptLanguage, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			q = f
		}
		if l, ok := Parse(tag); ok && q > 0 {
			prefs = append(prefs, pref{l, q})
		}
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })
	if len(prefs) > 0 {
		return prefs[0].lang
	}
	return EN
}

// Localizer is implemented by values that can render themselves in a language.
type Localizer interface {
	In(Lang) string
}

// Message is an English format string and its arguments. The zero Message is empty.
type Message struct {
	format string
	args   []any
}

// M makes a message. Arguments that are messages, errors or other Localizers are rendered in the
// target language too.
func M(format string, args ...any) Message { return Message{format: format, args: args} }

// With returns the message with args in place of its own.
func (m Message) With(args ...any) Message { return Message{format: m.format, args: args} }

// String renders the message in English.
func (m Message) String() string { return m.In(EN) }

// IsZero reports whether the message is empty.
func (m Message) IsZero() bool { return m.format == "" }

// In renders the message in l.
func (m Message) In(l Lang) string {
	if m.format == "" {
		return ""
	}
	return render(l, m.format, m.args)
}

// T renders a format string and its arguments in l.
func T(l Lang, format string, args ...any) string { return render(l, format, args) }

func render(l Lang, format string, args []any) string {
	f := format
	if l == ZH {
		if t, ok := zh[format]; ok {
			f = t
		}
	}
	f = strings.ReplaceAll(f, "%w", "%v")
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case Localizer:
			out[i] = v.In(l)
		case error:
			out[i] = Localize(v, l)
		default:
			out[i] = a
		}
	}
	return fmt.Sprintf(f, out...)
}

// Error is an error whose message can be localised. Error() is English.
type Error struct {
	code    string
	msg     Message
	wrapped []error
}

// Errorf makes a localisable error. %w wraps an error as fmt.Errorf does.
func Errorf(format string, args ...any) error { return newError("", format, args) }

// Coded makes a localisable error with a stable code that API clients can match on.
func Coded(code, format string, args ...any) error { return newError(code, format, args) }

func newError(code, format string, args []any) *Error {
	e := &Error{code: code, msg: M(format, args...)}
	if strings.Contains(format, "%w") {
		for _, a := range args {
			if err, ok := a.(error); ok {
				e.wrapped = append(e.wrapped, err)
			}
		}
	}
	return e
}

func (e *Error) Error() string    { return e.msg.In(EN) }
func (e *Error) In(l Lang) string { return e.msg.In(l) }
func (e *Error) Unwrap() []error  { return e.wrapped }
func (e *Error) Code() string     { return e.code }
func (e *Error) Message() Message { return e.msg }

// Localize renders err in l: a Localizer renders itself, an errors.Join renders each error on its own
// line, and anything else falls back to err.Error().
func Localize(err error, l Lang) string {
	if err == nil {
		return ""
	}
	if lz, ok := err.(Localizer); ok {
		return lz.In(l)
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var lines []string
		for _, e := range j.Unwrap() {
			if e != nil {
				lines = append(lines, Localize(e, l))
			}
		}
		return strings.Join(lines, "\n")
	}
	return err.Error()
}

// Code returns the first non-empty code in err's chain, or "".
func Code(err error) string {
	if err == nil {
		return ""
	}
	if c, ok := err.(interface{ Code() string }); ok && c.Code() != "" {
		return c.Code()
	}
	switch u := err.(type) { //nolint:errorlint // Code walks the chain itself
	case interface{ Unwrap() error }:
		return Code(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if c := Code(e); c != "" {
				return c
			}
		}
	}
	return ""
}
