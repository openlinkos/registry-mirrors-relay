package ops

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// UserView 是一个客户端账号。
type UserView struct {
	Name         string    `json:"name"`
	Source       string    `json:"source"` // config（users_file）| store
	Disabled     bool      `json:"disabled"`
	ManifestRate float64   `json:"manifest_rate,omitempty"`
	BlobStreams  int       `json:"blob_streams,omitempty"`
	Note         string    `json:"note,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	CreatedBy    string    `json:"created_by,omitempty"`
	LastSeen     time.Time `json:"last_seen,omitzero"` // 自启动以来最后一次请求
}

// Users 列出客户端账号，按名字排序。
func (o *Ops) Users() []UserView {
	rt := o.app.Runtime()
	stored := map[string]store.ClientUser{}
	for _, u := range rt.Data.Users {
		stored[u.Name] = u
	}
	seen := o.app.Stats.LastSeen()
	out := []UserView{}
	for _, u := range rt.Users {
		v := UserView{Name: u.Name, Source: u.Source, Disabled: u.Disabled, ManifestRate: u.ManifestRPS,
			BlobStreams: u.BlobStreams, LastSeen: seen[u.Name]}
		if su, ok := stored[u.Name]; ok && u.Source == "store" {
			v.Note, v.Created, v.CreatedBy = su.Note, su.Created, su.CreatedBy
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// User 返回一个客户端账号。
func (o *Ops) User(name string) (UserView, bool) {
	for _, u := range o.Users() {
		if u.Name == name {
			return u, true
		}
	}
	return UserView{}, false
}

// IPRuleView 是一条 IP 规则。
type IPRuleView struct {
	CIDR      string    `json:"cidr"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// ImageRuleView 是一条镜像规则。
type ImageRuleView struct {
	Pattern   string    `json:"pattern"`
	Kind      string    `json:"kind"`
	Status    int       `json:"status,omitempty"`
	Message   string    `json:"message,omitempty"`
	Source    string    `json:"source"`
	Note      string    `json:"note,omitempty"`
	Hits      uint64    `json:"hits"` // 自启动以来拒绝的次数（只有禁止规则有）
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Rules 是全部访问规则。
type Rules struct {
	IP     []IPRuleView    `json:"ip"`
	Bans   []store.Ban     `json:"bans"`
	Images []ImageRuleView `json:"images"`
}

// Rules 列出 IP 规则、生效中的封禁和镜像规则。配置文件里的在前。
func (o *Ops) Rules() Rules {
	rt := o.app.Runtime()
	hits := o.app.Stats.RuleHits()
	r := Rules{IP: []IPRuleView{}, Bans: []store.Ban{}, Images: []ImageRuleView{}}
	for _, c := range rt.Config.Access.IPAllow {
		r.IP = append(r.IP, IPRuleView{CIDR: c, Kind: "allow", Source: "config"})
	}
	for _, c := range rt.Config.Access.IPDeny {
		r.IP = append(r.IP, IPRuleView{CIDR: c, Kind: "deny", Source: "config"})
	}
	for _, ir := range rt.Data.IPRules {
		r.IP = append(r.IP, IPRuleView{CIDR: ir.CIDR, Kind: ir.Kind, Source: "store", Note: ir.Note, Created: ir.Created, CreatedBy: ir.CreatedBy})
	}
	for _, p := range rt.Config.Access.Images.Allow {
		r.Images = append(r.Images, ImageRuleView{Pattern: p, Kind: "allow", Source: "config"})
	}
	for _, d := range rt.Config.Access.Images.Deny {
		r.Images = append(r.Images, ImageRuleView{Pattern: d.Pattern, Kind: "deny", Status: d.Status, Message: d.Message, Source: "config", Hits: hits[d.Pattern]})
	}
	for _, ir := range rt.Data.ImageRules {
		v := ImageRuleView{Pattern: ir.Pattern, Kind: ir.Kind, Status: ir.Status, Message: ir.Message, Source: "store", Note: ir.Note,
			Created: ir.Created, CreatedBy: ir.CreatedBy}
		if ir.Kind == "deny" {
			v.Hits = hits[ir.Pattern]
		}
		r.Images = append(r.Images, v)
	}
	now := time.Now()
	for _, b := range rt.Data.Bans {
		if now.Before(b.Until) {
			r.Bans = append(r.Bans, b)
		}
	}
	sort.Slice(r.Bans, func(i, j int) bool { return r.Bans[i].Until.Before(r.Bans[j].Until) })
	return r
}

// ImageDecision 是"这个镜像能不能拉"的结果。
type ImageDecision struct {
	Image   string `json:"image"` // 补全后的名字：上游/仓库
	Allowed bool   `json:"allowed"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Rule    string `json:"rule"`
}

// TestImage 按 docker pull 的写法补全镜像名（没写上游时是 Docker Hub，补 library/），再按镜像规则判断。
func (o *Ops) TestImage(image string) ImageDecision {
	name := strings.ToLower(strings.TrimSpace(image))
	if i := strings.LastIndexAny(name, ":@"); i > strings.LastIndex(name, "/") {
		name = name[:i] // 去掉 tag 或 digest
	}
	rt := o.app.Runtime()
	first, rest, ok := strings.Cut(name, "/")
	up, known := rt.Router.Upstream(first)
	if !ok || !strings.ContainsAny(first, ".:") || !known {
		up, rest = config.DefaultUpstream, name
	}
	if u := rt.Config.Upstreams[up]; u != nil && u.LibraryPrefix && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	full := up + "/" + rest
	allowed, status, msg, rule := rt.Images.Decide(full)
	return ImageDecision{Image: full, Allowed: allowed, Status: status, Message: msg, Rule: rule}
}

// UpstreamView 是一个上游的设置和状态。
type UpstreamView struct {
	Name          string           `json:"name"`
	Registry      string           `json:"registry"`
	BreakerOpen   bool             `json:"breaker_open"`
	RedirectHosts []string         `json:"redirect_hosts"`       // 生效的完整白名单（正则）
	AddedHosts    []string         `json:"added_redirect_hosts"` // 在后台加的精确主机名
	UnlistedHosts map[string]int   `json:"unlisted_redirect_hosts"`
	Unlisted      string           `json:"unlisted_redirect"` // pass | deny
	Credentials   bool             `json:"credentials"`
	Health        *upstream.Health `json:"health,omitempty"`
	Quota         *upstream.Quota  `json:"quota,omitempty"`
}

// Upstreams 列出启用的上游，按名字排序。
func (o *Ops) Upstreams() []UpstreamView {
	rt := o.app.Runtime()
	unlisted := o.app.Stats.Unlisted()
	health := o.app.Health()
	out := []UpstreamView{}
	for _, n := range rt.Pool.Names() {
		c := rt.Config.Upstreams[n]
		hosts := append(append([]string{}, c.RedirectHosts...), c.ExtraRedirectHosts...)
		for _, h := range rt.Data.ExtraRedirectHosts[n] {
			hosts = append(hosts, regexp.QuoteMeta(h)) // 后台加的主机按字面匹配
		}
		v := UpstreamView{
			Name: n, Registry: c.Registry, BreakerOpen: rt.Pool.Get(n).BreakerOpen(), RedirectHosts: hosts,
			AddedHosts: append([]string{}, rt.Data.ExtraRedirectHosts[n]...), UnlistedHosts: unlisted[n],
			Unlisted: c.UnlistedRedirect, Credentials: c.Credentials != nil, Quota: rt.Pool.Get(n).Quota(),
		}
		if h, ok := health[n]; ok {
			v.Health = &h
		}
		out = append(out, v)
	}
	return out
}

// TokenView 是一个管理 API 令牌（不含令牌本身）。
type TokenView struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Source    string    `json:"source"`
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Tokens 列出管理 API 令牌，配置文件里的在前。
func (o *Ops) Tokens() []TokenView {
	rt := o.app.Runtime()
	out := []TokenView{}
	for _, t := range rt.Config.Admin.APITokens {
		out = append(out, TokenView{Name: t.Name, Role: t.Role, Source: "config"})
	}
	for _, t := range rt.Data.APITokens {
		out = append(out, TokenView{Name: t.Name, Role: t.Role, Source: "store", Created: t.Created, CreatedBy: t.CreatedBy})
	}
	return out
}

// AdminView 是一个后台管理员（不含口令哈希）。
type AdminView struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Disabled   bool      `json:"disabled"`
	MustChange bool      `json:"must_change"` // 第一次登录后必须先改口令
	Source     string    `json:"source"`      // store | config（运营数据只读时，来自 admin.initial_admin）
	Created    time.Time `json:"created,omitzero"`
}

// Admins 列出后台管理员。
func (o *Ops) Admins() []AdminView {
	out := []AdminView{}
	data := o.app.Store.Data()
	for _, a := range data.Admins {
		out = append(out, AdminView{Name: a.Name, Role: a.Role, Disabled: a.Disabled, MustChange: a.MustChange, Source: "store", Created: a.Created})
	}
	if ia := o.app.Runtime().Config.Admin.InitialAdmin; len(out) == 0 && !o.app.Store.Writable() && ia.Username != "" {
		out = append(out, AdminView{Name: ia.Username, Role: "admin", Source: "config"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SiteView 是生效的站点信息，以及它来自哪里。
type SiteView struct {
	Name      string            `json:"name"`
	Operator  string            `json:"operator,omitempty"`
	Links     map[string]string `json:"links,omitempty"`
	Source    string            `json:"source"` // config | store
	Updated   time.Time         `json:"updated,omitzero"`
	UpdatedBy string            `json:"updated_by,omitempty"`
}

// Site 返回生效的站点信息。
func (o *Ops) Site() SiteView {
	rt := o.app.Runtime()
	v := SiteView{Name: rt.Site.Name, Operator: rt.Site.Operator, Links: rt.Site.Links, Source: "config"}
	if s := rt.Data.Site; s != nil {
		v.Source, v.Updated, v.UpdatedBy = "store", s.Updated, s.UpdatedBy
	}
	return v
}

// SiteLinkKeys 是站点信息里可以写的链接，按用法页页脚的顺序。
var SiteLinkKeys = []string{"status", "terms", "privacy", "abuse", "source", "home"}
