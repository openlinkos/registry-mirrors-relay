// Package ops holds the management operations: validate the input, change the operational data,
// write the audit log and rebuild the Runtime. Both the management API and the web console call
// into it, so there is only one copy of the validation rules.
package ops

import (
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

// Actor is the person or program performing an operation; it is written to the audit log.
type Actor struct {
	Kind string // token (API token) | admin (console login) | cli | system
	Name string
	IP   netip.Addr
}

func (a Actor) String() string { return a.Kind + ":" + a.Name }

// Error is an error that can be shown to the operator, with a suggested HTTP status code. Key is a
// stable code for API clients; the messages are in messages.go. Error() is English; the console and the
// API render it in the reader's language with i18n.Localize.
type Error struct {
	Status int
	Key    string
	Msg    i18n.Message
	Cause  error // appended to the message, for example what is wrong in the configuration file
}

// NewError makes an *Error with the message for key; args fill in its verbs.
func NewError(status int, key string, args ...any) *Error {
	m, ok := messages[key]
	if !ok {
		m = i18n.M("unknown error %s", key)
		args = nil
	}
	if len(args) > 0 {
		m = m.With(args...)
	}
	return &Error{Status: status, Key: key, Msg: m}
}

func (e *Error) Error() string { return e.In(i18n.EN) }
func (e *Error) Code() string  { return e.Key }
func (e *Error) Unwrap() error { return e.Cause }

// In renders the error in l.
func (e *Error) In(l i18n.Lang) string {
	if e.Cause == nil {
		return e.Msg.In(l)
	}
	return i18n.T(l, "%s: %v", e.Msg, e.Cause)
}

func bad(key string) error       { return NewError(http.StatusBadRequest, key) }
func notFound(key string) error  { return NewError(http.StatusNotFound, key) }
func conflict(key string) error  { return NewError(http.StatusConflict, key) }
func forbidden(key string) error { return NewError(http.StatusForbidden, key) }

// Status returns the HTTP status code for an error: an *Error uses its own, anything else is 500.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return http.StatusInternalServerError
}

// Ops performs management operations.
type Ops struct {
	app *app.App
	// OnAdminChange is called after a console administrator's password, role or disabled state changes,
	// or after the administrator is deleted (the web console uses it to end their sessions).
	OnAdminChange func(name string)
}

// New creates an Ops.
func New(a *app.App) *Ops { return &Ops{app: a} }

// App returns the underlying App (for read-only display).
func (o *Ops) App() *app.App { return o.app }

// Audit writes an audit log entry.
func (o *Ops) Audit(who Actor, action, target, detail string) {
	ip := ""
	if who.IP.IsValid() {
		ip = who.IP.String()
	}
	_ = o.app.Store.Audit(store.AuditEntry{Actor: who.String(), IP: ip, Action: action, Target: target, Detail: truncate(detail, 300)})
}

// change changes the operational data, writes the audit log and rebuilds the Runtime.
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
		return i18n.Errorf("saved, but applying the change failed: %w", err)
	}
	return nil
}

// Writable reports whether the operational data can be changed.
func (o *Ops) Writable() bool { return o.app.Store.Writable() }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// GeneratePassword generates a random password (26 base32 characters, 130 bits).
func GeneratePassword() string { return rand.Text() }

// HostAllowed reports whether the admin console and API may be accessed with this Host.
//   - admin.public (the default): any domain is fine. The console is open to every address anyway, DNS
//     rebinding only gets the login page, and there is no session cookie under another domain.
//   - admin.public: false: only the host of public_url, or a DirectHost (this machine, SSH forwarding,
//     kubectl port-forward, opening it by IP on the LAN). Every other domain is refused, so that DNS
//     rebinding cannot expose an internal-only console to outside web pages.
func HostAllowed(a config.Admin, r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if a.Public {
		return host != "" && !strings.Contains(host, "%")
	}
	if publicURL := a.PublicURL; publicURL != "" {
		if u, err := url.Parse(publicURL); err == nil && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return DirectHost(host)
}

// DirectHost reports whether the Host (a port is allowed) is localhost or an IP address. DNS rebinding
// relies on a domain name, so it does not apply when accessing by IP; IPv6 with a zone is not accepted.
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

// LoopbackHost reports whether the Host (a port is allowed) is localhost or a loopback address.
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
