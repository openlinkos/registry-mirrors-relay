package admin

import (
	"fmt"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
)

// tr translates interface text. Arguments come in pairs, a name and a value, and replace {name} in the
// text. English is the source: a key missing from the Chinese catalogue falls back to English, and a key
// missing from both shows as the key itself (TestMessagesComplete catches both).
func tr(lang i18n.Lang, key string, args ...any) string {
	text, ok := messagesEN[key]
	if lang == i18n.ZH {
		if t, found := messagesZH[key]; found {
			text, ok = t, true
		}
	}
	if !ok {
		return key
	}
	for i := 0; i+1 < len(args); i += 2 {
		name := fmt.Sprint(args[i])
		text = strings.ReplaceAll(text, "{"+name+"}", fmt.Sprint(args[i+1]))
	}
	return text
}

// localize renders messages in lang.
func localize(lang i18n.Lang, ms []i18n.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.In(lang)
	}
	return out
}
