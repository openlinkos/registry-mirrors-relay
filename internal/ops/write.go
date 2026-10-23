package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/access"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// BcryptCost 是新口令的 bcrypt 强度（客户端账号和后台管理员都用它）。
const BcryptCost = 12

var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// ValidName 判断账号、管理员、令牌的名字是否合法。
func ValidName(s string) bool { return nameRe.MatchString(s) }

// CheckPassword 检查口令长度：12 到 72 个字节（bcrypt 只看前 72 个字节）。
func CheckPassword(p string) error {
	if n := len(p); n < 12 || n > 72 {
		return bad("password.length")
	}
	return nil
}

// HashPassword 用 bcrypt 生成口令哈希。
func HashPassword(p string) (string, error) {
	if err := CheckPassword(p); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), BcryptCost)
	return string(h), err
}

// ---- 客户端账号

// UserInput 是新建或修改客户端账号的输入。为 nil 的字段表示不改。
type UserInput struct {
	Name         string   `json:"name"`
	Password     *string  `json:"password,omitempty"`
	Hash         *string  `json:"hash,omitempty"` // 现成的 bcrypt 哈希
	Disabled     *bool    `json:"disabled,omitempty"`
	ManifestRate *float64 `json:"manifest_rate,omitempty"`
	BlobStreams  *int     `json:"blob_streams,omitempty"`
	Note         *string  `json:"note,omitempty"`
}

// hash 从口令或现成的 bcrypt 哈希得到要存的哈希，都没给时返回空串。
func (in UserInput) hash() (string, error) {
	switch {
	case in.Password != nil && in.Hash != nil:
		return "", bad("user.password_or_hash")
	case in.Password != nil:
		return HashPassword(*in.Password)
	case in.Hash != nil:
		if !access.ValidHash(*in.Hash) {
			return "", bad("user.hash")
		}
		return *in.Hash, nil
	}
	return "", nil
}

func (in UserInput) apply(u *store.ClientUser) error {
	if in.Disabled != nil {
		u.Disabled = *in.Disabled
	}
	if in.ManifestRate != nil {
		if *in.ManifestRate < 0 || *in.ManifestRate > 1000 {
			return bad("limit.manifest_rate")
		}
		u.ManifestRate = *in.ManifestRate
	}
	if in.BlobStreams != nil {
		if *in.BlobStreams < 0 || *in.BlobStreams > 10000 {
			return bad("limit.blob_streams")
		}
		u.BlobStreams = *in.BlobStreams
	}
	if in.Note != nil {
		if len(*in.Note) > 500 {
			return bad("note.length")
		}
		u.Note = strings.TrimSpace(*in.Note)
	}
	return nil
}

// CreateUser 新建客户端账号。
func (o *Ops) CreateUser(who Actor, in UserInput) error {
	if !nameRe.MatchString(in.Name) {
		return bad("user.name")
	}
	if u, ok := o.app.Runtime().Users[in.Name]; ok && u.Source == "config" {
		return conflict("user.in_file")
	}
	h, err := in.hash()
	if err != nil {
		return err
	}
	if h == "" {
		return bad("user.password_required")
	}
	return o.change(who, "user.create", in.Name, "", func(d *store.Data) error {
		for _, u := range d.Users {
			if u.Name == in.Name {
				return conflict("user.exists")
			}
		}
		u := store.ClientUser{Name: in.Name, Hash: h, Created: time.Now().UTC(), CreatedBy: who.String()}
		if err := in.apply(&u); err != nil {
			return err
		}
		d.Users = append(d.Users, u)
		return nil
	})
}

// UpdateUser 修改客户端账号（in.Name 不用）。
func (o *Ops) UpdateUser(who Actor, name string, in UserInput) error {
	h, err := in.hash()
	if err != nil {
		return err
	}
	var detail []string
	if h != "" {
		detail = append(detail, "password")
	}
	if in.Disabled != nil {
		detail = append(detail, "disabled="+strconv.FormatBool(*in.Disabled))
	}
	if in.ManifestRate != nil || in.BlobStreams != nil {
		detail = append(detail, "limits")
	}
	if in.Note != nil {
		detail = append(detail, "note")
	}
	return o.change(who, "user.update", name, strings.Join(detail, ","), func(d *store.Data) error {
		for i := range d.Users {
			if d.Users[i].Name == name {
				if h != "" {
					d.Users[i].Hash = h
				}
				return in.apply(&d.Users[i])
			}
		}
		return notFound("user.not_found")
	})
}

// DeleteUser 删除客户端账号。
func (o *Ops) DeleteUser(who Actor, name string) error {
	return o.change(who, "user.delete", name, "", func(d *store.Data) error {
		for i, u := range d.Users {
			if u.Name == name {
				d.Users = append(d.Users[:i], d.Users[i+1:]...)
				return nil
			}
		}
		return notFound("user.not_found")
	})
}

// ---- IP 规则与封禁

// NormCIDR 把 IP 或 CIDR 规范成 CIDR，拒绝 /0。
func NormCIDR(s string) (string, error) {
	ps, err := config.ParsePrefixes([]string{strings.TrimSpace(s)})
	if err != nil || len(ps) != 1 {
		return "", bad("cidr.invalid")
	}
	if ps[0].Bits() == 0 {
		return "", bad("cidr.all")
	}
	return ps[0].String(), nil
}

func checkKind(kind string) error {
	if kind != "allow" && kind != "deny" {
		return bad("kind.invalid")
	}
	return nil
}

// AddIPRule 加一条 IP 规则，返回规范后的 CIDR。
func (o *Ops) AddIPRule(who Actor, cidr, kind, note string) (string, error) {
	cidr, err := NormCIDR(cidr)
	if err != nil {
		return "", err
	}
	if err := checkKind(kind); err != nil {
		return "", err
	}
	if len(note) > 500 {
		return "", bad("note.length")
	}
	return cidr, o.change(who, "rule.ip.add", cidr, kind, func(d *store.Data) error {
		for _, ir := range d.IPRules {
			if ir.CIDR == cidr && ir.Kind == kind {
				return conflict("rule.exists")
			}
		}
		d.IPRules = append(d.IPRules, store.IPRule{CIDR: cidr, Kind: kind, Note: strings.TrimSpace(note), Created: time.Now().UTC(), CreatedBy: who.String()})
		return nil
	})
}

// DeleteIPRule 删除一条 IP 规则。
func (o *Ops) DeleteIPRule(who Actor, cidr, kind string) error {
	cidr, err := NormCIDR(cidr)
	if err != nil {
		return err
	}
	return o.change(who, "rule.ip.delete", cidr, kind, func(d *store.Data) error {
		for i, ir := range d.IPRules {
			if ir.CIDR == cidr && ir.Kind == kind {
				d.IPRules = append(d.IPRules[:i], d.IPRules[i+1:]...)
				return nil
			}
		}
		return notFound("rule.not_found")
	})
}

// ParseBanDuration 解析 30m、2h、7d 这样的期限，最长 365 天。
func ParseBanDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	d, err := time.ParseDuration(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		days, err = strconv.Atoi(n)
		d = time.Duration(days) * 24 * time.Hour
	}
	if err != nil || d < time.Minute || d > 365*24*time.Hour {
		return 0, bad("ban.duration")
	}
	return d, nil
}

// AddBan 临时封禁一个地址或地址段。同一段已有封禁时替换它。
func (o *Ops) AddBan(who Actor, cidr, duration, reason string) (string, time.Time, error) {
	cidr, err := NormCIDR(cidr)
	if err != nil {
		return "", time.Time{}, err
	}
	d, err := ParseBanDuration(duration)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(reason) > 500 {
		return "", time.Time{}, bad("ban.reason")
	}
	now := time.Now()
	until := now.Add(d).UTC().Truncate(time.Second)
	return cidr, until, o.change(who, "ban.add", cidr, duration+" "+reason, func(data *store.Data) error {
		kept := data.Bans[:0]
		for _, bn := range data.Bans {
			if bn.CIDR != cidr && now.Before(bn.Until) { // 顺便清掉过期的
				kept = append(kept, bn)
			}
		}
		data.Bans = append(kept, store.Ban{CIDR: cidr, Until: until, Reason: strings.TrimSpace(reason), Created: now.UTC(), CreatedBy: who.String()})
		return nil
	})
}

// DeleteBan 解除封禁。
func (o *Ops) DeleteBan(who Actor, cidr string) error {
	cidr, err := NormCIDR(cidr)
	if err != nil {
		return err
	}
	return o.change(who, "ban.delete", cidr, "", func(d *store.Data) error {
		for i, bn := range d.Bans {
			if bn.CIDR == cidr {
				d.Bans = append(d.Bans[:i], d.Bans[i+1:]...)
				return nil
			}
		}
		return notFound("ban.not_found")
	})
}

// ---- 镜像规则

// ImageRuleInput 是新建镜像规则的输入。
type ImageRuleInput struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"kind"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Note    string `json:"note"`
}

// AddImageRule 加一条镜像规则，返回规范后的写法。
func (o *Ops) AddImageRule(who Actor, in ImageRuleInput) (string, error) {
	in.Pattern = strings.TrimSpace(in.Pattern)
	if _, err := access.CompileImagePattern(in.Pattern); err != nil {
		return "", bad("image.pattern")
	}
	if reg, _, _ := strings.Cut(in.Pattern, "/"); reg != "*" {
		if _, ok := o.app.Runtime().Config.Upstreams[strings.ToLower(reg)]; !ok {
			return "", bad("image.upstream")
		}
	}
	switch in.Kind {
	case "allow":
		in.Status, in.Message = 0, ""
	case "deny":
		if in.Status == 0 {
			in.Status = 403
		}
		if in.Status != 403 && in.Status != 451 {
			return "", bad("image.status")
		}
	default:
		return "", checkKind(in.Kind)
	}
	if len(in.Message) > 300 || len(in.Note) > 500 {
		return "", bad("image.text")
	}
	return in.Pattern, o.change(who, "rule.image.add", in.Pattern, in.Kind, func(d *store.Data) error {
		for _, ir := range d.ImageRules {
			if ir.Pattern == in.Pattern && ir.Kind == in.Kind {
				return conflict("rule.exists")
			}
		}
		d.ImageRules = append(d.ImageRules, store.ImageRule{Pattern: in.Pattern, Kind: in.Kind, Status: in.Status,
			Message: strings.TrimSpace(in.Message), Note: strings.TrimSpace(in.Note), Created: time.Now().UTC(), CreatedBy: who.String()})
		return nil
	})
}

// DeleteImageRule 删除一条镜像规则。
func (o *Ops) DeleteImageRule(who Actor, pattern, kind string) error {
	return o.change(who, "rule.image.delete", pattern, kind, func(d *store.Data) error {
		for i, ir := range d.ImageRules {
			if ir.Pattern == pattern && ir.Kind == kind {
				d.ImageRules = append(d.ImageRules[:i], d.ImageRules[i+1:]...)
				return nil
			}
		}
		return notFound("rule.not_found")
	})
}

// ---- 上游

// AddRedirectHost 把一个精确主机名加进上游的重定向白名单。
func (o *Ops) AddRedirectHost(who Actor, name, host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if _, ok := o.app.Runtime().Config.Upstreams[name]; !ok {
		return "", notFound("upstream.not_found")
	}
	if !hostnameRe.MatchString(host) {
		return "", bad("host.invalid")
	}
	return host, o.change(who, "upstream.redirect_host.add", name, host, func(d *store.Data) error {
		if d.ExtraRedirectHosts == nil {
			d.ExtraRedirectHosts = map[string][]string{}
		}
		for _, h := range d.ExtraRedirectHosts[name] {
			if h == host {
				return conflict("host.exists")
			}
		}
		d.ExtraRedirectHosts[name] = append(d.ExtraRedirectHosts[name], host)
		return nil
	})
}

// DeleteRedirectHost 把后台加的主机从白名单里去掉。
func (o *Ops) DeleteRedirectHost(who Actor, name, host string) error {
	host = strings.ToLower(host)
	return o.change(who, "upstream.redirect_host.delete", name, host, func(d *store.Data) error {
		hs := d.ExtraRedirectHosts[name]
		for i, h := range hs {
			if h == host {
				d.ExtraRedirectHosts[name] = append(hs[:i], hs[i+1:]...)
				if len(d.ExtraRedirectHosts[name]) == 0 {
					delete(d.ExtraRedirectHosts, name)
				}
				return nil
			}
		}
		return notFound("host.not_found")
	})
}

// CheckUpstream 立即探测一个上游的 /v2/。
func (o *Ops) CheckUpstream(ctx context.Context, who Actor, name string) (upstream.Health, error) {
	h, ok := o.app.CheckUpstream(ctx, name)
	if !ok {
		return h, notFound("upstream.not_found")
	}
	detail := "ok"
	if !h.OK {
		detail = h.Error
	}
	o.Audit(who, "upstream.check", name, detail)
	return h, nil
}

// ---- 令牌

// CreateToken 新建管理 API 令牌，返回令牌本身（只在这时给一次）。
func (o *Ops) CreateToken(who Actor, name, role string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", bad("user.name")
	}
	if role != "admin" && role != "viewer" {
		return "", bad("role.invalid")
	}
	for _, t := range o.app.Runtime().Config.Admin.APITokens {
		if t.Name == name {
			return "", conflict("token.in_config")
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "rmr_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, o.change(who, "token.create", name, role, func(d *store.Data) error {
		for _, t := range d.APITokens {
			if t.Name == name {
				return conflict("token.exists")
			}
		}
		d.APITokens = append(d.APITokens, store.APIToken{Name: name, Hash: hex.EncodeToString(sum[:]), Role: role,
			Created: time.Now().UTC(), CreatedBy: who.String()})
		return nil
	})
}

// DeleteToken 吊销管理 API 令牌。
func (o *Ops) DeleteToken(who Actor, name string) error {
	return o.change(who, "token.delete", name, "", func(d *store.Data) error {
		for i, t := range d.APITokens {
			if t.Name == name {
				d.APITokens = append(d.APITokens[:i], d.APITokens[i+1:]...)
				return nil
			}
		}
		return notFound("token.not_found")
	})
}

// ---- 后台管理员

// AdminInput 是修改后台管理员的输入。为 nil 的字段表示不改。
type AdminInput struct {
	Password *string `json:"password,omitempty"`
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

func activeAdmins(d *store.Data) int {
	n := 0
	for _, a := range d.Admins {
		if a.Role == "admin" && !a.Disabled {
			n++
		}
	}
	return n
}

// CreateAdmin 新建后台管理员。口令是创建的人设的，所以新管理员第一次登录后必须先改口令。
func (o *Ops) CreateAdmin(who Actor, name, password, role string) error {
	if !nameRe.MatchString(name) {
		return bad("user.name")
	}
	if role != "admin" && role != "viewer" {
		return bad("role.invalid")
	}
	if strings.EqualFold(password, name) {
		return bad("password.username")
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	return o.change(who, "admin.create", name, role, func(d *store.Data) error {
		for _, a := range d.Admins {
			if a.Name == name {
				return conflict("admin.exists")
			}
		}
		d.Admins = append(d.Admins, store.Admin{Name: name, Hash: h, Role: role, MustChange: true, Created: time.Now().UTC()})
		return nil
	})
}

// UpdateAdmin 修改后台管理员。不能让系统里一个能用的 admin 角色都不剩，也不能停用自己或降低自己的角色。
// 重设别人的口令后，那个管理员下次登录必须先改口令；改自己的口令则解除这个要求。
func (o *Ops) UpdateAdmin(who Actor, name string, in AdminInput) error {
	self := who.Kind == "admin" && who.Name == name
	h := ""
	if in.Password != nil {
		if strings.EqualFold(*in.Password, name) {
			return bad("password.username")
		}
		var err error
		if h, err = HashPassword(*in.Password); err != nil {
			return err
		}
	}
	if in.Role != nil && *in.Role != "admin" && *in.Role != "viewer" {
		return bad("role.invalid")
	}
	if self && ((in.Disabled != nil && *in.Disabled) || (in.Role != nil && *in.Role != "admin")) {
		return forbidden("admin.self")
	}
	var detail []string
	if h != "" {
		detail = append(detail, "password")
	}
	if in.Role != nil {
		detail = append(detail, "role="+*in.Role)
	}
	if in.Disabled != nil {
		detail = append(detail, "disabled="+strconv.FormatBool(*in.Disabled))
	}
	err := o.change(who, "admin.update", name, strings.Join(detail, ","), func(d *store.Data) error {
		for i := range d.Admins {
			a := &d.Admins[i]
			if a.Name != name {
				continue
			}
			if h != "" {
				// 必须改口令时不能原样再设一遍（那样配置里或别人知道的口令仍然有效）
				if self && bcrypt.CompareHashAndPassword([]byte(a.Hash), []byte(*in.Password)) == nil {
					return bad("password.same")
				}
				a.Hash, a.MustChange = h, !self
			}
			if in.Role != nil {
				a.Role = *in.Role
			}
			if in.Disabled != nil {
				a.Disabled = *in.Disabled
			}
			if activeAdmins(d) == 0 {
				return conflict("admin.last")
			}
			return nil
		}
		return notFound("admin.not_found")
	})
	if err == nil && o.OnAdminChange != nil {
		o.OnAdminChange(name)
	}
	return err
}

// DeleteAdmin 删除后台管理员。不能删除自己，也不能删掉最后一个能用的 admin 角色。
func (o *Ops) DeleteAdmin(who Actor, name string) error {
	if who.Kind == "admin" && who.Name == name {
		return forbidden("admin.delete_self")
	}
	err := o.change(who, "admin.delete", name, "", func(d *store.Data) error {
		for i, a := range d.Admins {
			if a.Name == name {
				d.Admins = append(d.Admins[:i], d.Admins[i+1:]...)
				if activeAdmins(d) == 0 {
					return conflict("admin.last")
				}
				return nil
			}
		}
		return notFound("admin.not_found")
	})
	if err == nil && o.OnAdminChange != nil {
		o.OnAdminChange(name)
	}
	return err
}

// ---- 站点信息

// SiteInput 是在后台设置的站点信息。
type SiteInput struct {
	Name     string            `json:"name"`
	Operator string            `json:"operator"`
	Links    map[string]string `json:"links"`
}

// SetSite 设置站点信息，整体代替配置文件里的 site。
func (o *Ops) SetSite(who Actor, in SiteInput) error {
	in.Name, in.Operator = strings.TrimSpace(in.Name), strings.TrimSpace(in.Operator)
	if in.Name == "" || len(in.Name) > 80 || len(in.Operator) > 120 {
		return bad("site.name")
	}
	links := map[string]string{}
	allowed := map[string]bool{}
	for _, k := range SiteLinkKeys {
		allowed[k] = true
	}
	for k, v := range in.Links {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !allowed[k] {
			return &Error{Status: http.StatusBadRequest, Key: "site.link_key", Arg: k}
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(v) > 500 {
			return &Error{Status: http.StatusBadRequest, Key: "site.link", Arg: k}
		}
		links[k] = v
	}
	return o.change(who, "site.update", in.Name, "", func(d *store.Data) error {
		d.Site = &store.Site{Name: in.Name, Operator: in.Operator, Links: links, Updated: time.Now().UTC(), UpdatedBy: who.String()}
		return nil
	})
}

// ResetSite 去掉后台设置的站点信息，改回用配置文件的。
func (o *Ops) ResetSite(who Actor) error {
	return o.change(who, "site.reset", "", "", func(d *store.Data) error {
		d.Site = nil
		return nil
	})
}

// ---- 配置

// Reload 重新加载配置文件。
func (o *Ops) Reload(who Actor) (*app.ReloadStatus, error) {
	err := o.app.ReloadConfig()
	detail := "ok"
	if err != nil {
		detail = err.Error()
	}
	o.Audit(who, "config.reload", "", detail)
	if err != nil {
		return o.app.LastReload(), &Error{Status: http.StatusUnprocessableEntity, Key: "reload.failed", Detail: err.Error()}
	}
	return o.app.LastReload(), nil
}
