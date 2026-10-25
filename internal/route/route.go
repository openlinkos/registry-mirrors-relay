// Package route maps requests to entrances and upstreams: it finds the entrance by listen address and Host,
// then parses the path according to the entrance type.
//
// There are two kinds of entrance:
//   - Fixed-upstream entrances (one subdomain or one port per upstream): the repository name in the path is
//     passed to that upstream as is, and the first segment is never taken as a registry; when containerd's
//     ns parameter does not match this upstream, the result is 404.
//   - Single-domain entrances: when the first path segment is an upstream name or alias from the
//     configuration, the upstream is chosen by that prefix (/v2/ghcr.io/owner/image/...); otherwise by the ns
//     parameter; when neither is present, the default upstream is used (this is the kind of request Docker's
//     registry-mirrors sends).
package route

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ref"
)

// Kind is the type of request.
type Kind int

const (
	KindPing      Kind = iota // /v2/
	KindManifest              // /v2/<name>/manifests/<ref>
	KindBlob                  // /v2/<name>/blobs/<digest>
	KindTags                  // /v2/<name>/tags/list
	KindReferrers             // /v2/<name>/referrers/<digest>
)

func (k Kind) String() string {
	return [...]string{"ping", "manifest", "blob", "tags", "referrers"}[k]
}

// Entrance is an entrance.
type Entrance struct {
	Name            string
	Upstream        string // fixed upstream; empty means a single-domain entrance
	DefaultUpstream string
	Auth            string
	Page            string // usage page: public | login | off
}

// Target is a parsed request.
type Target struct {
	Kind     Kind
	Upstream string     // upstream name, such as docker.io
	Repo     string     // repository name on the upstream side, with library/ already added
	Ref      string     // tag or digest
	Client   string     // name in the client's request (with prefix, without added library/), used to rewrite pagination links
	Query    url.Values // query parameters allowed to be forwarded to the upstream
}

// Error is the reason parsing failed, mapped to an OCI error code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func notFound(msg string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "NAME_UNKNOWN", Message: msg}
}

func invalid(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "NAME_INVALID", Message: msg}
}

// Router is immutable and replaced as a whole when the configuration is reloaded.
type Router struct {
	exact   map[string]*Entrance // Host → entrance
	any     *Entrance            // the entrance whose hosts include "*"
	names   map[string]string    // upstream names and aliases → upstream name (enabled upstreams only)
	library map[string]bool
}

// New builds a Router from the configuration. The configuration must already be validated.
func New(cfg *config.Config) *Router {
	r := &Router{exact: map[string]*Entrance{}, names: map[string]string{}, library: map[string]bool{}}
	for name, u := range cfg.Upstreams {
		if u.Disabled {
			continue
		}
		r.names[name] = name
		for _, a := range u.Aliases {
			r.names[a] = name
		}
		r.library[name] = u.LibraryPrefix
	}
	for _, ce := range cfg.Entrances {
		e := &Entrance{Name: ce.Name, Upstream: ce.Upstream, DefaultUpstream: ce.DefaultUpstream, Auth: ce.Auth, Page: ce.Page}
		for _, h := range ce.Hosts {
			if h == "*" {
				r.any = e
			} else {
				r.exact[h] = e
			}
		}
	}
	return r
}

// NormalizeHost strips the port, square brackets and trailing dot, and converts to lower case.
func NormalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// Entrance finds the entrance for a Host, or returns nil if there is none.
func (r *Router) Entrance(host string) *Entrance {
	if e := r.exact[NormalizeHost(host)]; e != nil {
		return e
	}
	return r.any
}

// Upstream resolves a name or alias to an upstream name.
func (r *Router) Upstream(name string) (string, bool) {
	n, ok := r.names[strings.ToLower(name)]
	return n, ok
}

var (
	itemRe = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs|referrers)/([^/]+)$`)
	tagsRe = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

// Parse parses a request under /v2. u is the request URL (r.URL).
func (r *Router) Parse(e *Entrance, u *url.URL) (Target, *Error) {
	path := u.Path
	if path == "/v2" || path == "/v2/" {
		return Target{Kind: KindPing}, nil
	}
	// Encoded paths (%2F and the like), empty segments, . and .., and non-ASCII characters are all rejected, so
	// that both sides cannot interpret the path differently
	if u.RawPath != "" || strings.Contains(path, "//") || hasDotSegment(path) || !isASCII(path) {
		return Target{}, invalid("invalid path")
	}
	var t Target
	var name string
	if m := itemRe.FindStringSubmatch(path); m != nil {
		name, t.Ref = m[1], m[3]
		switch m[2] {
		case "manifests":
			t.Kind = KindManifest
			if !ref.ValidTag(t.Ref) && !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID", Message: "invalid tag or digest"}
			}
		case "blobs":
			t.Kind = KindBlob
			if !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "invalid digest"}
			}
		case "referrers":
			t.Kind = KindReferrers
			if !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "invalid digest"}
			}
		}
	} else if m := tagsRe.FindStringSubmatch(path); m != nil {
		name, t.Kind = m[1], KindTags
	} else {
		return Target{}, notFound("unsupported endpoint")
	}
	t.Client = name

	q := u.Query()
	ns := strings.ToLower(q.Get("ns"))
	if e.Upstream != "" {
		// Fixed-upstream entrance: use the name as is, ignore the first segment
		if ns != "" {
			if up, ok := r.Upstream(ns); !ok || up != e.Upstream {
				return Target{}, notFound("this entrance only proxies " + e.Upstream)
			}
		}
		t.Upstream, t.Repo = e.Upstream, name
	} else {
		first, rest, hasSlash := strings.Cut(name, "/")
		if hasSlash && looksLikeRegistry(first) {
			up, ok := r.Upstream(first)
			if !ok {
				return Target{}, notFound("upstream not configured: " + first)
			}
			if ns != "" {
				if nup, ok := r.Upstream(ns); !ok || nup != up {
					return Target{}, notFound("path prefix and ns point to different upstreams")
				}
			}
			t.Upstream, t.Repo = up, rest
		} else {
			up := e.DefaultUpstream
			if ns != "" {
				var ok bool
				if up, ok = r.Upstream(ns); !ok {
					return Target{}, notFound("upstream not configured: " + ns)
				}
			}
			t.Upstream, t.Repo = up, name
		}
	}
	if r.library[t.Upstream] && !strings.Contains(t.Repo, "/") {
		t.Repo = "library/" + t.Repo
	}
	if !ref.ValidName(t.Repo) {
		return Target{}, invalid("invalid repository name")
	}
	t.Query = forwardQuery(t.Kind, q)
	return t, nil
}

// looksLikeRegistry follows Docker's rule: the first segment is a registry only if it contains . or :, or is
// localhost. Docker Hub namespaces cannot contain dots, so there is no ambiguity.
func looksLikeRegistry(s string) bool {
	return strings.ContainsAny(s, ".:") || s == "localhost"
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// forwardQuery keeps only the parameters the upstream needs: n and last for tags/list, artifactType for
// referrers. ns and the like are not forwarded.
func forwardQuery(k Kind, q url.Values) url.Values {
	out := url.Values{}
	switch k {
	case KindTags:
		if n := q.Get("n"); n != "" {
			if v, err := strconv.Atoi(n); err == nil && v > 0 {
				out.Set("n", strconv.Itoa(min(v, 1000)))
			}
		}
		if last := q.Get("last"); last != "" && len(last) <= 256 {
			out.Set("last", last)
		}
	case KindReferrers:
		if at := q.Get("artifactType"); at != "" && len(at) <= 255 {
			out.Set("artifactType", at)
		}
	}
	return out
}

// UpstreamPath is the path sent to the upstream (without query parameters).
func (t Target) UpstreamPath() string {
	switch t.Kind {
	case KindManifest:
		return "/v2/" + t.Repo + "/manifests/" + t.Ref
	case KindBlob:
		return "/v2/" + t.Repo + "/blobs/" + t.Ref
	case KindReferrers:
		return "/v2/" + t.Repo + "/referrers/" + t.Ref
	case KindTags:
		return "/v2/" + t.Repo + "/tags/list"
	}
	return "/v2/"
}

// RewriteLink rewrites the Link header of upstream tags/list pagination to the path the client uses.
// Only links to tags/list of the same repository are accepted; others are dropped (an empty string is returned).
func (t Target) RewriteLink(link string) string {
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start != 0 || end < 0 {
		return ""
	}
	u, err := url.Parse(link[1:end])
	if err != nil || u.Path != "/v2/"+t.Repo+"/tags/list" {
		return ""
	}
	q := forwardQuery(KindTags, u.Query())
	s := "</v2/" + t.Client + "/tags/list"
	if enc := q.Encode(); enc != "" {
		s += "?" + enc
	}
	return s + ">" + link[end+1:]
}
