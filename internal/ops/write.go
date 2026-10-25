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

// BcryptCost is the bcrypt cost for new passwords (used for both client accounts and console
// administrators).
const BcryptCost = 12

var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// ValidName reports whether a client account, administrator or token name is valid.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// CheckPassword checks the password length: 12 to 72 bytes (bcrypt only looks at the first 72 bytes).
func CheckPassword(p string) error {
	if n := len(p); n < 12 || n > 72 {
		return bad("password.length")
	}
	return nil
}

// HashPassword generates a password hash with bcrypt.
func HashPassword(p string) (string, error) {
	if err := CheckPassword(p); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), BcryptCost)
	return string(h), err
}

// ---- Client accounts

// UserInput is the input for creating or changing a client account. A nil field means no change.
type UserInput struct {
	Name         string   `json:"name"`
	Password     *string  `json:"password,omitempty"`
	Hash         *string  `json:"hash,omitempty"` // a ready-made bcrypt hash
	Disabled     *bool    `json:"disabled,omitempty"`
	ManifestRate *float64 `json:"manifest_rate,omitempty"`
	BlobStreams  *int     `json:"blob_streams,omitempty"`
	Note         *string  `json:"note,omitempty"`
}

// hash returns the hash to store, from the password or a ready-made bcrypt hash; it returns an empty
// string when neither is given.
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

// CreateUser creates a client account.
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

// UpdateUser changes a client account (in.Name is not used).
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

// DeleteUser deletes a client account.
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

// ---- IP rules and bans

// NormCIDR normalises an IP address or CIDR range to a CIDR and refuses /0.
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

// AddIPRule adds an IP rule and returns the normalised CIDR.
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

// DeleteIPRule deletes an IP rule.
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

// ParseBanDuration parses a duration such as 30m, 2h or 7d, up to 365 days.
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

// AddBan temporarily bans an address or address range. An existing ban on the same range is replaced.
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
			if bn.CIDR != cidr && now.Before(bn.Until) { // clear out expired ones while at it
				kept = append(kept, bn)
			}
		}
		data.Bans = append(kept, store.Ban{CIDR: cidr, Until: until, Reason: strings.TrimSpace(reason), Created: now.UTC(), CreatedBy: who.String()})
		return nil
	})
}

// DeleteBan lifts a ban.
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

// ---- Image rules

// ImageRuleInput is the input for creating an image rule.
type ImageRuleInput struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"kind"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Note    string `json:"note"`
}

// AddImageRule adds an image rule and returns the normalised pattern.
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

// DeleteImageRule deletes an image rule.
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

// ---- Upstreams

// AddRedirectHost adds an exact host name to an upstream's redirect allowlist.
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

// DeleteRedirectHost removes a host added in the console from the allowlist.
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

// CheckUpstream probes an upstream's /v2/ immediately.
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

// ---- Tokens

// CreateToken creates a management API token and returns the token itself (given only this once).
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

// DeleteToken revokes a management API token.
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

// ---- Console administrators

// AdminInput is the input for changing a console administrator. A nil field means no change.
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

// CreateAdmin creates a console administrator. The password is set by whoever creates the account, so
// the new administrator must change it after the first login.
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

// UpdateAdmin changes a console administrator. It must not leave the system without an enabled
// administrator with the admin role, and administrators cannot disable themselves or lower their own
// role. After someone else's password is reset, that administrator must change it at the next login;
// changing your own password clears this requirement.
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
				// when the password must be changed, it cannot be set to the same one again (the password from
				// the configuration, or one someone else knows, would stay valid)
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

// DeleteAdmin deletes a console administrator. Administrators cannot delete themselves, and the last
// enabled administrator with the admin role cannot be deleted.
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

// ---- Site information

// SiteInput is the site information set in the console.
type SiteInput struct {
	Name     string            `json:"name"`
	Operator string            `json:"operator"`
	Links    map[string]string `json:"links"`
}

// SetSite sets the site information, replacing the site section of the configuration file as a whole.
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

// ResetSite removes the site information set in the console and goes back to the configuration file's.
func (o *Ops) ResetSite(who Actor) error {
	return o.change(who, "site.reset", "", "", func(d *store.Data) error {
		d.Site = nil
		return nil
	})
}

// ---- Configuration

// Reload reloads the configuration file.
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
