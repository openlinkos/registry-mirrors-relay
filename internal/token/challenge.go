// Package token 在服务端向上游的令牌服务取令牌，并缓存到过期前。客户端看不到上游的认证质询。
package token

import "strings"

// Challenge 是 WWW-Authenticate 里的一个质询。
type Challenge struct {
	Scheme string // 小写：bearer、basic
	Params map[string]string
}

// Realm、Service、Scope 是 Bearer 质询的常用参数。
func (c Challenge) Realm() string   { return c.Params["realm"] }
func (c Challenge) Service() string { return c.Params["service"] }
func (c Challenge) Scope() string   { return c.Params["scope"] }

// ParseChallenges 解析一个或多个 WWW-Authenticate 头。参数值可以带引号（支持 \ 转义），也可以不带
// （gcr.io 的 service=gcr.io 就不带）。一个头里可以有多个质询，用逗号分开。
func ParseChallenges(values []string) []Challenge {
	var out []Challenge
	for _, v := range values {
		out = append(out, parseOne(v)...)
	}
	return out
}

func parseOne(s string) []Challenge {
	var out []Challenge
	var cur *Challenge
	i := 0
	skip := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
	}
	token := func() string {
		start := i
		for i < len(s) && !strings.ContainsRune(" \t,=\"", rune(s[i])) {
			i++
		}
		return s[start:i]
	}
	for {
		skip()
		if i >= len(s) {
			break
		}
		name := token()
		if name == "" {
			i++ // 不认识的字符，跳过
			continue
		}
		// 后面紧跟 = 的是参数，否则是新质询的 scheme
		j := i
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j < len(s) && s[j] == '=' && cur != nil {
			i = j + 1
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			var val string
			if i < len(s) && s[i] == '"' {
				i++
				var sb strings.Builder
				for i < len(s) && s[i] != '"' {
					if s[i] == '\\' && i+1 < len(s) {
						i++
					}
					sb.WriteByte(s[i])
					i++
				}
				i++ // 结尾的引号
				val = sb.String()
			} else {
				val = token()
			}
			cur.Params[strings.ToLower(name)] = val
			continue
		}
		out = append(out, Challenge{Scheme: strings.ToLower(name), Params: map[string]string{}})
		cur = &out[len(out)-1]
	}
	return out
}
