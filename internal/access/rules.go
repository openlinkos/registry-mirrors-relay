package access

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Ban is a temporary ban.
type Ban struct {
	Prefix netip.Prefix
	Until  time.Time
	Reason string
}

// IPRules decides whether a client IP may access the service. Immutable.
type IPRules struct {
	allow []netip.Prefix
	deny  []netip.Prefix
	bans  []Ban
}

// NewIPRules creates the rules. An empty allow means no restriction on source addresses.
func NewIPRules(allow, deny []netip.Prefix, bans []Ban) *IPRules {
	return &IPRules{allow: allow, deny: deny, bans: bans}
}

// Decide returns whether access is allowed, and the reason for a denial (written to the log, not returned to
// the client).
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

// ImageRule is a compiled image rule.
type ImageRule struct {
	Pattern string
	Status  int // status code of a deny rule: 403 or 451
	Message string
	re      *regexp.Regexp
}

// CompileImagePattern compiles a wildcard pattern that starts with the upstream into a regular expression:
// * matches any characters within one segment, ** matches any number of segments (including zero), and * as
// the first segment means any upstream. For example ghcr.io/evil/**, */library/nginx, docker.io/bitnami/*.
func CompileImagePattern(p string) (*regexp.Regexp, error) {
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return nil, fmt.Errorf("%q must start with the upstream", p)
	}
	var b strings.Builder
	b.WriteString("^")
	for i, s := range segs {
		switch s {
		case "**":
			if i == 0 {
				return nil, fmt.Errorf("%q: the upstream segment cannot be **", p)
			}
			b.WriteString("(?:/.*)?")
			continue
		case "":
			return nil, fmt.Errorf("%q has an empty segment", p)
		}
		if i > 0 {
			b.WriteString("/")
		}
		for _, part := range strings.Split(s, "*") {
			b.WriteString(regexp.QuoteMeta(strings.ToLower(part)))
			b.WriteString("[^/]*")
		}
		// Remove the extra trailing [^/]*
		str := b.String()
		b.Reset()
		b.WriteString(strings.TrimSuffix(str, "[^/]*"))
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// ImageRules decides whether an image may be pulled. Immutable.
type ImageRules struct {
	allow []*regexp.Regexp
	deny  []ImageRule
}

// NewImageRules compiles the rules. An empty allow means no restriction.
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

// Decide checks a full image name (upstream/repository, such as docker.io/library/nginx). When the image is
// not allowed, it returns the status code and a message for the client.
func (r *ImageRules) Decide(full string) (ok bool, status int, message string, rule string) {
	if r == nil {
		return true, 0, "", ""
	}
	full = strings.ToLower(full)
	for _, d := range r.deny {
		if d.re.MatchString(full) {
			msg := d.Message
			if msg == "" {
				msg = "this image cannot be pulled through this service"
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
	return false, 403, "this service only allows pulling specified images", "allow"
}
