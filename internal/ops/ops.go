// Package ops 是管理操作：校验输入、修改运营数据、记审计、重新生成 Runtime。
// 管理 API 和网页后台都调用这里，两边的校验规则因此只有一份。
package ops

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

// Actor 是执行操作的人或程序，写进审计。
type Actor struct {
	Kind string // token（API 令牌）| admin（后台登录）| cli | system
	Name string
	IP   netip.Addr
}

func (a Actor) String() string { return a.Kind + ":" + a.Name }

// Error 是可以告诉操作者的错误，带建议的 HTTP 状态码。说明见 messages.go，Error() 是中文。
type Error struct {
	Status int
	Key    string
	Arg    string // 说明里的 {v}
	Detail string // 附在说明后面的原始错误（例如配置文件哪里写错了）
}

func (e *Error) Error() string { return e.Message("zh") }

func bad(key string) error       { return &Error{Status: http.StatusBadRequest, Key: key} }
func notFound(key string) error  { return &Error{Status: http.StatusNotFound, Key: key} }
func conflict(key string) error  { return &Error{Status: http.StatusConflict, Key: key} }
func forbidden(key string) error { return &Error{Status: http.StatusForbidden, Key: key} }

// Status 返回错误对应的 HTTP 状态码：*Error 用它自己的，其他一律 500。
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return http.StatusInternalServerError
}

// Ops 执行管理操作。
type Ops struct {
	app *app.App
	// OnAdminChange 在后台管理员的口令、角色、停用状态改变或被删除后调用（网页后台用来结束他的会话）。
	OnAdminChange func(name string)
}

// New 创建 Ops。
func New(a *app.App) *Ops { return &Ops{app: a} }

// App 返回底层的 App（只读的展示用）。
func (o *Ops) App() *app.App { return o.app }

// Audit 记一条审计。
func (o *Ops) Audit(who Actor, action, target, detail string) {
	ip := ""
	if who.IP.IsValid() {
		ip = who.IP.String()
	}
	_ = o.app.Store.Audit(store.AuditEntry{Actor: who.String(), IP: ip, Action: action, Target: target, Detail: truncate(detail, 300)})
}

// change 修改运营数据、记审计、重新生成 Runtime。
func (o *Ops) change(who Actor, action, target, detail string, fn func(*store.Data) error) error {
	err := o.app.Store.Update(fn)
	switch {
	case errors.Is(err, store.ErrReadOnly):
		return conflict("store.readonly")
	case err != nil:
		return err
	}
	o.Audit(who, action, target, detail)
	if err := o.app.Rebuild(); err != nil {
		return fmt.Errorf("已保存，但应用时出错：%w", err)
	}
	return nil
}

// Writable 表示运营数据能不能改。
func (o *Ops) Writable() bool { return o.app.Store.Writable() }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// GeneratePassword 生成一个随机口令（26 个字符的 base32，130 位）。
func GeneratePassword() string { return rand.Text() }

// HostAllowed 判断管理后台和 API 能不能用这个 Host 访问：和 public_url 的主机一致，或者是 DirectHost
// （本机、SSH 转发、kubectl port-forward、局域网里用 IP 直接打开）。其他域名一律拒绝，防 DNS 重绑定。
// 谁能连进来由管理入口的访问范围（admin.allow）决定，见 mgmt 包；这里只防浏览器被别的网站利用。
func HostAllowed(publicURL string, r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if publicURL != "" {
		if u, err := url.Parse(publicURL); err == nil && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return DirectHost(host)
}

// DirectHost 判断 Host（可以带端口）是不是 localhost 或一个 IP 地址。DNS 重绑定要靠域名，用 IP 访问时不存在；
// 带 zone 的 IPv6 不接受。
func DirectHost(host string) bool {
	if LoopbackHost(host) {
		return true
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && a.Zone() == ""
}

// LoopbackHost 判断 Host（可以带端口）是不是 localhost 或回环地址。
func LoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}
