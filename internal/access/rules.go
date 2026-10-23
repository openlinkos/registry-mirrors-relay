package access

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Ban 是一次临时封禁。
type Ban struct {
	Prefix netip.Prefix
	Until  time.Time
	Reason string
}

// IPRules 决定一个客户端 IP 能不能访问。不可变。
type IPRules struct {
	allow []netip.Prefix
	deny  []netip.Prefix
	bans  []Ban
}

// NewIPRules 创建规则。allow 为空表示不限制来源。
func NewIPRules(allow, deny []netip.Prefix, bans []Ban) *IPRules {
	return &IPRules{allow: allow, deny: deny, bans: bans}
}

// Decide 返回是否允许，以及拒绝的原因（写进日志，不返回给客户端）。
func (r *IPRules) Decide(ip netip.Addr, now time.Time) (bool, string) {
	if r == nil {
		return true, ""
	}
	ip = ip.Unmap()
	for _, b := range r.bans {
		if b.Prefix.Contains(ip) && now.Before(b.Until) {
			return false, "banned"
		}
	}
	for _, p := range r.deny {
		if p.Contains(ip) {
			return false, "ip_deny"
		}
	}
	if len(r.allow) == 0 {
		return true, ""
	}
	for _, p := range r.allow {
		if p.Contains(ip) {
			return true, ""
		}
	}
	return false, "ip_allow"
}

// ImageRule 是一条编译好的镜像规则。
type ImageRule struct {
	Pattern string
	Status  int // 禁止规则的状态码：403 或 451
	Message string
	re      *regexp.Regexp
}

// CompileImagePattern 把带上游的通配写法编译成正则：* 匹配一段里的任意字符，** 匹配任意多段（含零段），
// 第一段写 * 表示任意上游。例如 ghcr.io/evil/**、*/library/nginx、docker.io/bitnami/*。
func CompileImagePattern(p string) (*regexp.Regexp, error) {
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return nil, fmt.Errorf("%q 要写全上游", p)
	}
	var b strings.Builder
	b.WriteString("^")
	for i, s := range segs {
		switch s {
		case "**":
			if i == 0 {
				return nil, fmt.Errorf("%q：上游那一段不能写 **", p)
			}
			b.WriteString("(?:/.*)?")
			continue
		case "":
			return nil, fmt.Errorf("%q 里有空的段", p)
		}
		if i > 0 {
			b.WriteString("/")
		}
		for _, part := range strings.Split(s, "*") {
			b.WriteString(regexp.QuoteMeta(strings.ToLower(part)))
			b.WriteString("[^/]*")
		}
		// 去掉最后多加的 [^/]*
		str := b.String()
		b.Reset()
		b.WriteString(strings.TrimSuffix(str, "[^/]*"))
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// ImageRules 决定一个镜像能不能拉。不可变。
type ImageRules struct {
	allow []*regexp.Regexp
	deny  []ImageRule
}

// NewImageRules 编译规则。allow 为空表示不限制。
func NewImageRules(allow []string, deny []ImageRule) (*ImageRules, error) {
	r := &ImageRules{}
	for _, p := range allow {
		re, err := CompileImagePattern(p)
		if err != nil {
			return nil, err
		}
		r.allow = append(r.allow, re)
	}
	for _, d := range deny {
		re, err := CompileImagePattern(d.Pattern)
		if err != nil {
			return nil, err
		}
		if d.Status == 0 {
			d.Status = 403
		}
		d.re = re
		r.deny = append(r.deny, d)
	}
	return r, nil
}

// Decide 判断完整的镜像名（上游/仓库，如 docker.io/library/nginx）。不允许时返回状态码和给客户端看的说明。
func (r *ImageRules) Decide(full string) (ok bool, status int, message string, rule string) {
	if r == nil {
		return true, 0, "", ""
	}
	full = strings.ToLower(full)
	for _, d := range r.deny {
		if d.re.MatchString(full) {
			msg := d.Message
			if msg == "" {
				msg = "这个镜像不能通过本服务拉取"
			}
			return false, d.Status, msg, d.Pattern
		}
	}
	if len(r.allow) == 0 {
		return true, 0, "", ""
	}
	for _, re := range r.allow {
		if re.MatchString(full) {
			return true, 0, "", ""
		}
	}
	return false, 403, "本服务只允许拉取指定的镜像", "allow"
}
