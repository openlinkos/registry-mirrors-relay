// Package store 存运营数据：客户端账号、IP 规则、临时封禁、镜像规则、API 令牌、后台管理员，以及审计日志和每小时的统计。
// 不存任何镜像数据。两种实现：
//   - bolt：一个 bbolt 文件，单实例可写；
//   - none：只读，运营数据全部来自配置文件（k8s 多副本时用）；审计和统计只在内存里保留一小段。
package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrReadOnly 表示运营数据是只读的。
var ErrReadOnly = errors.New("运营数据是只读的（admin.store.driver: none），请改配置文件")

// ClientUser 是一个客户端账号（拉取时 docker login 用的）。
type ClientUser struct {
	Name         string    `json:"name"`
	Hash         string    `json:"hash"` // bcrypt
	Disabled     bool      `json:"disabled,omitempty"`
	ManifestRate float64   `json:"manifest_rate,omitempty"` // 单独的限流，0 表示用默认值
	BlobStreams  int       `json:"blob_streams,omitempty"`
	Note         string    `json:"note,omitempty"`
	Created      time.Time `json:"created"`
	CreatedBy    string    `json:"created_by,omitempty"`
}

// IPRule 是一条 IP 白名单或黑名单。
type IPRule struct {
	CIDR      string    `json:"cidr"`
	Kind      string    `json:"kind"` // allow | deny
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Ban 是一次临时封禁。
type Ban struct {
	CIDR      string    `json:"cidr"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// ImageRule 是一条镜像白名单或黑名单。
type ImageRule struct {
	Pattern   string    `json:"pattern"`
	Kind      string    `json:"kind"`             // allow | deny
	Status    int       `json:"status,omitempty"` // deny：403 或 451
	Message   string    `json:"message,omitempty"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// APIToken 是管理 API 的令牌。只存 SHA-256，明文只在创建时给一次。
type APIToken struct {
	Name      string    `json:"name"`
	Hash      string    `json:"hash"`
	Role      string    `json:"role"` // admin | viewer
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Admin 是后台管理员。
type Admin struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash"` // bcrypt
	Role     string    `json:"role"` // admin | viewer
	Disabled bool      `json:"disabled,omitempty"`
	Created  time.Time `json:"created"`
}

// Data 是全部运营数据。很小（通常几 KB），整体读写。
type Data struct {
	Users      []ClientUser `json:"users,omitempty"`
	IPRules    []IPRule     `json:"ip_rules,omitempty"`
	Bans       []Ban        `json:"bans,omitempty"`
	ImageRules []ImageRule  `json:"image_rules,omitempty"`
	APITokens  []APIToken   `json:"api_tokens,omitempty"`
	Admins     []Admin      `json:"admins,omitempty"`
	// ExtraRedirectHosts 是在后台加进白名单的重定向主机（精确主机名，不是正则），按上游分
	ExtraRedirectHosts map[string][]string `json:"extra_redirect_hosts,omitempty"`
	Updated            time.Time           `json:"updated"`
}

// Clone 深拷贝。
func (d Data) Clone() Data {
	b, _ := json.Marshal(d)
	var c Data
	_ = json.Unmarshal(b, &c)
	return c
}

// AuditEntry 是一条审计记录：谁、什么时候、做了什么。
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	IP     string    `json:"ip,omitempty"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Store 是运营数据的存储。
type Store interface {
	// Data 返回当前数据的副本。
	Data() Data
	// Update 在副本上执行 fn，成功后整体写入。只读存储返回 ErrReadOnly。
	Update(fn func(*Data) error) error
	// Audit 记一条审计。只读存储也会记（在内存里）。
	Audit(e AuditEntry) error
	// AuditLog 返回最近的审计，最新的在前。
	AuditLog(limit int) []AuditEntry
	// SaveHour 保存一个小时的统计（JSON），HourStats 读回。
	SaveHour(hour time.Time, b []byte) error
	HourStats(since time.Time) [][]byte
	Writable() bool
	Close() error
}

// 保留上限
const (
	maxAudit    = 10000
	keepHours   = 90 * 24
	memAudit    = 1000
	memKeepHour = 48
)
