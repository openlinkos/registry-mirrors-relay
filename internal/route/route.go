// Package route 把请求映射到入口和上游：按监听地址和 Host 找入口，再按入口类型解析路径。
//
// 入口有两种：
//   - 固定上游的入口（每个上游一个子域名或一个端口）：路径里的仓库名原样交给这个上游，
//     不会把第一段当成 registry；containerd 的 ns 参数和这个上游不一致时返回 404。
//   - 单域名入口：路径第一段是配置里的上游名或别名时按前缀选上游（/v2/ghcr.io/owner/image/...）；
//     否则按 ns 参数选；都没有时用默认上游（Docker 的 registry-mirrors 发来的就是这种请求）。
package route

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ref"
)

// Kind 是请求的类型。
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

// Entrance 是一个入口。
type Entrance struct {
	Name            string
	Upstream        string // 固定上游；为空表示单域名入口
	DefaultUpstream string
	Auth            string
}

// Target 是解析后的请求。
type Target struct {
	Kind     Kind
	Upstream string     // 上游名，如 docker.io
	Repo     string     // 上游那边的仓库名，已补 library/
	Ref      string     // tag 或 digest
	Client   string     // 客户端请求里的名字（含前缀、未补 library/），改写分页链接时用
	Query    url.Values // 允许转发给上游的查询参数
}

// Error 是解析失败的原因，对应 OCI 的错误码。
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

// MainListener 是 listen.http 和 listen.https 共用的键；按端口区分的入口用自己的监听地址作键。
const MainListener = ""

type hostTable struct {
	exact map[string]*Entrance
	any   *Entrance
}

// Router 是不可变的，配置重载时整体替换。
type Router struct {
	listeners map[string]*hostTable
	names     map[string]string // 上游名和别名 → 上游名（只含启用的上游）
	library   map[string]bool
}

// New 由配置构建 Router。配置应已通过校验。
func New(cfg *config.Config) *Router {
	r := &Router{listeners: map[string]*hostTable{}, names: map[string]string{}, library: map[string]bool{}}
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
		e := &Entrance{Name: ce.Name, Upstream: ce.Upstream, DefaultUpstream: ce.DefaultUpstream, Auth: ce.Auth}
		key := MainListener
		if ce.Listen != "" {
			key = ce.Listen
		}
		t := r.listeners[key]
		if t == nil {
			t = &hostTable{exact: map[string]*Entrance{}}
			r.listeners[key] = t
		}
		hosts := ce.Hosts
		if len(hosts) == 0 {
			hosts = []string{"*"}
		}
		for _, h := range hosts {
			if h == "*" {
				t.any = e
			} else {
				t.exact[h] = e
			}
		}
	}
	return r
}

// NormalizeHost 去掉端口、方括号和末尾的点，转成小写。
func NormalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// Entrance 按监听地址和 Host 找入口，找不到返回 nil。
func (r *Router) Entrance(listener, host string) *Entrance {
	t := r.listeners[listener]
	if t == nil {
		return nil
	}
	if e := t.exact[NormalizeHost(host)]; e != nil {
		return e
	}
	return t.any
}

// Upstream 把名字或别名解析成上游名。
func (r *Router) Upstream(name string) (string, bool) {
	n, ok := r.names[strings.ToLower(name)]
	return n, ok
}

var (
	itemRe = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs|referrers)/([^/]+)$`)
	tagsRe = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

// Parse 解析 /v2 下的请求。u 是请求的 URL（r.URL）。
func (r *Router) Parse(e *Entrance, u *url.URL) (Target, *Error) {
	path := u.Path
	if path == "/v2" || path == "/v2/" {
		return Target{Kind: KindPing}, nil
	}
	// 编码过的路径（%2F 之类）、空段、. 和 ..、非 ASCII 字符一律拒绝，避免两边对路径的理解不一致
	if u.RawPath != "" || strings.Contains(path, "//") || hasDotSegment(path) || !isASCII(path) {
		return Target{}, invalid("路径不合法")
	}
	var t Target
	var name string
	if m := itemRe.FindStringSubmatch(path); m != nil {
		name, t.Ref = m[1], m[3]
		switch m[2] {
		case "manifests":
			t.Kind = KindManifest
			if !ref.ValidTag(t.Ref) && !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID", Message: "tag 或 digest 不合法"}
			}
		case "blobs":
			t.Kind = KindBlob
			if !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "digest 不合法"}
			}
		case "referrers":
			t.Kind = KindReferrers
			if !ref.ValidDigest(t.Ref) {
				return Target{}, &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "digest 不合法"}
			}
		}
	} else if m := tagsRe.FindStringSubmatch(path); m != nil {
		name, t.Kind = m[1], KindTags
	} else {
		return Target{}, notFound("不支持的接口")
	}
	t.Client = name

	q := u.Query()
	ns := strings.ToLower(q.Get("ns"))
	if e.Upstream != "" {
		// 固定上游的入口：名字原样用，不看第一段
		if ns != "" {
			if up, ok := r.Upstream(ns); !ok || up != e.Upstream {
				return Target{}, notFound("这个入口只代理 " + e.Upstream)
			}
		}
		t.Upstream, t.Repo = e.Upstream, name
	} else {
		first, rest, hasSlash := strings.Cut(name, "/")
		if hasSlash && looksLikeRegistry(first) {
			up, ok := r.Upstream(first)
			if !ok {
				return Target{}, notFound("没有配置上游 " + first)
			}
			if ns != "" {
				if nup, ok := r.Upstream(ns); !ok || nup != up {
					return Target{}, notFound("路径前缀和 ns 指向不同的上游")
				}
			}
			t.Upstream, t.Repo = up, rest
		} else {
			up := e.DefaultUpstream
			if ns != "" {
				var ok bool
				if up, ok = r.Upstream(ns); !ok {
					return Target{}, notFound("没有配置上游 " + ns)
				}
			}
			t.Upstream, t.Repo = up, name
		}
	}
	if r.library[t.Upstream] && !strings.Contains(t.Repo, "/") {
		t.Repo = "library/" + t.Repo
	}
	if !ref.ValidName(t.Repo) {
		return Target{}, invalid("仓库名不合法")
	}
	t.Query = forwardQuery(t.Kind, q)
	return t, nil
}

// looksLikeRegistry 沿用 Docker 的规则：第一段含 . 或 :，或者是 localhost，才是 registry。
// Docker Hub 的命名空间不能含点，所以不会混淆。
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

// forwardQuery 只保留上游需要的参数：tags/list 的 n、last，referrers 的 artifactType。ns 等不转发。
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

// UpstreamPath 是发给上游的路径（不含查询参数）。
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

// RewriteLink 把上游 tags/list 分页的 Link 头改成客户端用的路径。
// 只接受指向同一仓库 tags/list 的链接，其余丢弃（返回空串）。
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

type listenerKey struct{}

// WithListener 记下请求来自哪个监听（MainListener 或按端口区分的入口的地址）。
func WithListener(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, listenerKey{}, key)
}

// ListenerFrom 取出 WithListener 记下的监听，没有时返回 MainListener。
func ListenerFrom(ctx context.Context) string {
	if v, ok := ctx.Value(listenerKey{}).(string); ok {
		return v
	}
	return MainListener
}
