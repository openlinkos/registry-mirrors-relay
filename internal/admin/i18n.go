package admin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 界面语言：明确选过的（Cookie）优先，其次是浏览器的 Accept-Language，都没有时用中文。
func negotiateLang(cookieValue, acceptLanguage string) string {
	if cookieValue == "zh" || cookieValue == "en" {
		return cookieValue
	}
	type pref struct {
		tag   string
		q     float64
		index int
	}
	var prefs []pref
	for i, part := range strings.Split(acceptLanguage, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			q = f
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" && q > 0 {
			prefs = append(prefs, pref{tag, q, i})
		}
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })
	for _, p := range prefs {
		base, _, _ := strings.Cut(p.tag, "-")
		switch base {
		case "zh":
			return "zh"
		case "en":
			return "en"
		}
	}
	return "zh"
}

func htmlLang(lang string) string {
	if lang == "en" {
		return "en"
	}
	return "zh-CN"
}

// tr 翻译界面文案。参数成对：名字、值，替换文案里的 {名字}。缺的键直接显示键名（测试会发现）。
func tr(lang, key string, args ...any) string {
	m := messagesZH
	if lang == "en" {
		m = messagesEN
	}
	text, ok := m[key]
	if !ok {
		return key
	}
	for i := 0; i+1 < len(args); i += 2 {
		name := fmt.Sprint(args[i])
		text = strings.ReplaceAll(text, "{"+name+"}", fmt.Sprint(args[i+1]))
	}
	return text
}
