package ops

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/status"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// UserView is a client account.
type UserView struct {
	Name         string    `json:"name"`
	Source       string    `json:"source"` // config (users_file) | store
	Disabled     bool      `json:"disabled"`
	ManifestRate float64   `json:"manifest_rate,omitempty"`
	BlobStreams  int       `json:"blob_streams,omitempty"`
	Note         string    `json:"note,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	CreatedBy    string    `json:"created_by,omitempty"`
	LastSeen     time.Time `json:"last_seen,omitzero"` // the last request since start-up
}

// Users lists the client accounts, sorted by name.
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

// User returns one client account.
func (o *Ops) User(name string) (UserView, bool) {
	for _, u := range o.Users() {
		if u.Name == name {
			return u, true
		}
	}
	return UserView{}, false
}

// IPRuleView is one IP rule.
type IPRuleView struct {
	CIDR      string    `json:"cidr"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// ImageRuleView is one image rule.
type ImageRuleView struct {
	Pattern   string    `json:"pattern"`
	Kind      string    `json:"kind"`
	Status    int       `json:"status,omitempty"`
	Message   string    `json:"message,omitempty"`
	Source    string    `json:"source"`
	Note      string    `json:"note,omitempty"`
	Hits      uint64    `json:"hits"` // times denied since start-up (deny rules only)
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Rules holds all the access rules.
type Rules struct {
	IP     []IPRuleView    `json:"ip"`
	Bans   []store.Ban     `json:"bans"`
	Images []ImageRuleView `json:"images"`
}

// Rules lists the IP rules, the active bans and the image rules. Those from the configuration file
// come first.
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

// ImageDecision is the answer to "may this image be pulled".
type ImageDecision struct {
	Image   string `json:"image"` // the completed name: upstream/repository
	Allowed bool   `json:"allowed"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Rule    string `json:"rule"`
}

// TestImage completes the image name the way docker pull does (Docker Hub when no upstream is given,
// adding library/), then checks it against the image rules.
func (o *Ops) TestImage(image string) ImageDecision {
	name := strings.ToLower(strings.TrimSpace(image))
	if i := strings.LastIndexAny(name, ":@"); i > strings.LastIndex(name, "/") {
		name = name[:i] // drop the tag or digest
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

// UpstreamView is the settings and state of one upstream.
type UpstreamView struct {
	Name          string           `json:"name"`
	Registry      string           `json:"registry"`
	BreakerOpen   bool             `json:"breaker_open"`
	RedirectHosts []string         `json:"redirect_hosts"`       // the full effective allowlist (regexes)
	AddedHosts    []string         `json:"added_redirect_hosts"` // exact host names added in the console
	UnlistedHosts map[string]int   `json:"unlisted_redirect_hosts"`
	Unlisted      string           `json:"unlisted_redirect"` // pass | deny
	Credentials   bool             `json:"credentials"`
	Health        *upstream.Health `json:"health,omitempty"`
	Quota         *upstream.Quota  `json:"quota,omitempty"`
	Hours         []status.Hour    `json:"hours"` // hourly probe results for the last 24 hours, oldest first
}

// Upstreams lists the enabled upstreams, sorted by name.
func (o *Ops) Upstreams() []UpstreamView {
	rt := o.app.Runtime()
	unlisted := o.app.Stats.Unlisted()
	health := o.app.Health()
	out := []UpstreamView{}
	for _, n := range rt.Pool.Names() {
		c := rt.Config.Upstreams[n]
		hosts := append(append([]string{}, c.RedirectHosts...), c.ExtraRedirectHosts...)
		for _, h := range rt.Data.ExtraRedirectHosts[n] {
			hosts = append(hosts, regexp.QuoteMeta(h)) // hosts added in the console match literally
		}
		v := UpstreamView{
			Name: n, Registry: c.Registry, BreakerOpen: rt.Pool.Get(n).BreakerOpen(), RedirectHosts: hosts,
			AddedHosts: append([]string{}, rt.Data.ExtraRedirectHosts[n]...), UnlistedHosts: unlisted[n],
			Unlisted: c.UnlistedRedirect, Credentials: c.Credentials != nil, Quota: rt.Pool.Get(n).Quota(),
			Hours: o.app.ProbeHours(n),
		}
		if h, ok := health[n]; ok {
			v.Health = &h
		}
		out = append(out, v)
	}
	return out
}

// TokenView is a management API token (without the token itself).
type TokenView struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Source    string    `json:"source"`
	Created   time.Time `json:"created,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Tokens lists the management API tokens, those from the configuration file first.
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

// AdminView is a console administrator (without the password hash).
type AdminView struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Disabled   bool      `json:"disabled"`
	MustChange bool      `json:"must_change"` // must change the password after the first login
	Source     string    `json:"source"`      // store | config (from admin.initial_admin when the operational data is read-only)
	Created    time.Time `json:"created,omitzero"`
}

// Admins lists the console administrators.
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

// SiteView is the effective site information and where it comes from.
type SiteView struct {
	Name      string            `json:"name"`
	Operator  string            `json:"operator,omitempty"`
	Links     map[string]string `json:"links,omitempty"`
	Source    string            `json:"source"` // config | store
	Updated   time.Time         `json:"updated,omitzero"`
	UpdatedBy string            `json:"updated_by,omitempty"`
	// ShowStatus: whether the usage page shows the service status; comes only from the configuration
	// file (site.show_status)
	ShowStatus bool `json:"show_status"`
}

// Site returns the effective site information.
func (o *Ops) Site() SiteView {
	rt := o.app.Runtime()
	v := SiteView{Name: rt.Site.Name, Operator: rt.Site.Operator, Links: rt.Site.Links, Source: "config", ShowStatus: rt.Site.ShowStatus}
	if s := rt.Data.Site; s != nil {
		v.Source, v.Updated, v.UpdatedBy = "store", s.Updated, s.UpdatedBy
	}
	return v
}

// SiteLinkKeys are the links the site information may contain, in the order of the usage page footer.
var SiteLinkKeys = []string{"status", "terms", "privacy", "abuse", "source", "home"}
