// Package token obtains tokens from the upstream's token service on the server side and caches them
// until they are about to expire. Clients never see the upstream's authentication challenges.
package token

import "strings"

// Challenge is one challenge in WWW-Authenticate.
type Challenge struct {
	Scheme string // lowercase: bearer, basic
	Params map[string]string
}

// Realm, Service and Scope are the common parameters of a Bearer challenge.
func (c Challenge) Realm() string   { return c.Params["realm"] }
func (c Challenge) Service() string { return c.Params["service"] }
func (c Challenge) Scope() string   { return c.Params["scope"] }

// ParseChallenges parses one or more WWW-Authenticate headers. Parameter values may be quoted (with \
// escapes) or unquoted (gcr.io's service=gcr.io is unquoted). One header may hold several challenges,
// separated by commas.
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
			i++ // unrecognised character, skip it
			continue
		}
		// A name followed by = is a parameter; otherwise it is the scheme of a new challenge
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
				i++ // closing quote
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
