// Package store keeps the operational data: client accounts, IP rules, temporary bans, image rules, API
// tokens and console administrators, plus the audit log and hourly statistics. It never stores image data.
// There are two implementations:
//   - bolt: a single bbolt file, writable by a single instance;
//   - none: read-only, all operational data comes from the configuration file (for multiple k8s replicas);
//     the audit log and statistics are only kept in memory for a short while.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrReadOnly means the operational data is read-only.
var ErrReadOnly = errors.New("operational data is read-only (admin.store.driver: none), edit the configuration file instead")

// ErrLocked means another process (usually the running service) has the operational database open.
var ErrLocked = errors.New("another process has the operational database open")

// ClientUser is a client account (the one used with docker login for pulls).
type ClientUser struct {
	Name         string    `json:"name"`
	Hash         string    `json:"hash"` // bcrypt
	Disabled     bool      `json:"disabled,omitempty"`
	ManifestRate float64   `json:"manifest_rate,omitempty"` // per-account rate limit, 0 means the default
	BlobStreams  int       `json:"blob_streams,omitempty"`
	Note         string    `json:"note,omitempty"`
	Created      time.Time `json:"created"`
	CreatedBy    string    `json:"created_by,omitempty"`
}

// IPRule is an IP allowlist or deny list entry.
type IPRule struct {
	CIDR      string    `json:"cidr"`
	Kind      string    `json:"kind"` // allow | deny
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Ban is a temporary ban.
type Ban struct {
	CIDR      string    `json:"cidr"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// ImageRule is an image allowlist or deny list entry.
type ImageRule struct {
	Pattern   string    `json:"pattern"`
	Kind      string    `json:"kind"`             // allow | deny
	Status    int       `json:"status,omitempty"` // deny: 403 or 451
	Message   string    `json:"message,omitempty"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// APIToken is a management API token. Only its SHA-256 is stored; the plain text is shown once, at creation.
type APIToken struct {
	Name      string    `json:"name"`
	Hash      string    `json:"hash"`
	Role      string    `json:"role"` // admin | viewer
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Admin is a console administrator.
type Admin struct {
	Name     string `json:"name"`
	Hash     string `json:"hash"` // bcrypt
	Role     string `json:"role"` // admin | viewer
	Disabled bool   `json:"disabled,omitempty"`
	// MustChange: the password was set by someone else (initial_admin in the configuration, or created or reset
	// by another administrator); after the first sign-in the password must be changed first, and until then
	// neither the console nor the API can be used
	MustChange bool      `json:"must_change,omitempty"`
	Created    time.Time `json:"created"`
}

// Site is the site information edited in the console. When set, it replaces the site in the configuration
// file as a whole (the usage page and console title both use it).
type Site struct {
	Name      string            `json:"name"`
	Operator  string            `json:"operator,omitempty"`
	Brand     string            `json:"brand,omitempty"`
	Links     map[string]string `json:"links,omitempty"`
	Updated   time.Time         `json:"updated"`
	UpdatedBy string            `json:"updated_by,omitempty"`
}

// ConfigOverride is one console override of the configuration (see config.Override), stored by key in
// Data.ConfigOverrides.
type ConfigOverride struct {
	Value string `json:"value"` // YAML for this section, only the fields that differ from the configuration file
	// Base is a fingerprint of these fields' values in the configuration file at save time
	// (config.OverrideBase). A mismatch means the configuration file has changed since.
	Base      string    `json:"base,omitempty"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Data is all the operational data. It is small (usually a few KB) and read and written as a whole.
type Data struct {
	Users      []ClientUser `json:"users,omitempty"`
	IPRules    []IPRule     `json:"ip_rules,omitempty"`
	Bans       []Ban        `json:"bans,omitempty"`
	ImageRules []ImageRule  `json:"image_rules,omitempty"`
	APITokens  []APIToken   `json:"api_tokens,omitempty"`
	Admins     []Admin      `json:"admins,omitempty"`
	// ExtraRedirectHosts are redirect hosts added to the allowlist in the console (exact host names, not
	// regular expressions), per upstream
	ExtraRedirectHosts map[string][]string `json:"extra_redirect_hosts,omitempty"`
	Site               *Site               `json:"site,omitempty"`
	// ConfigOverrides are the console overrides of the configuration, keyed by config.Override's Key
	ConfigOverrides map[string]ConfigOverride `json:"config_overrides,omitempty"`
	Updated         time.Time                 `json:"updated"`
}

// Clone returns a deep copy.
func (d Data) Clone() Data {
	b, _ := json.Marshal(d)
	var c Data
	_ = json.Unmarshal(b, &c)
	return c
}

// AuditEntry is an audit log entry: who did what, and when.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	IP     string    `json:"ip,omitempty"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Store is the operational data store.
type Store interface {
	// Data returns a copy of the current data.
	Data() Data
	// Update runs fn on a copy and writes it as a whole on success. A read-only store returns ErrReadOnly.
	Update(fn func(*Data) error) error
	// Audit records an audit log entry. A read-only store records it too (in memory).
	Audit(e AuditEntry) error
	// AuditLog returns the most recent audit log entries, newest first.
	AuditLog(limit int) []AuditEntry
	// SaveHour saves one hour of statistics (JSON); HourStats reads them back.
	SaveHour(hour time.Time, b []byte) error
	HourStats(since time.Time) [][]byte
	Writable() bool
	Close() error
}

// Retention limits
const (
	maxAudit    = 10000
	keepHours   = 90 * 24
	memAudit    = 1000
	memKeepHour = 48
)
