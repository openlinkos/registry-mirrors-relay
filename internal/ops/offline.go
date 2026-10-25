package ops

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

// Offline changes administrators directly in an operational database that no service has open: the admin
// subcommands use it while the service is stopped. It applies the same rules as Ops.
type Offline struct {
	Store store.Store
}

// Admins lists the administrators.
func (o Offline) Admins() []AdminView {
	out := []AdminView{}
	for _, a := range o.Store.Data().Admins {
		out = append(out, AdminView{Name: a.Name, Role: a.Role, Disabled: a.Disabled, MustChange: a.MustChange, Source: "store", Created: a.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CreateAdmin creates an administrator. A password set from the command line need not be changed at
// the first sign-in.
func (o Offline) CreateAdmin(who Actor, name, password, role string) error {
	h, err := checkNewAdmin(name, password, role)
	if err != nil {
		return err
	}
	return o.change(who, "admin.create", name, role, func(d *store.Data) error {
		return addAdmin(d, store.Admin{Name: name, Hash: h, Role: role, Created: time.Now().UTC()})
	})
}

// SetPassword sets an administrator's password and enables the account.
func (o Offline) SetPassword(who Actor, name, password string) error {
	if strings.EqualFold(password, name) {
		return bad("password.username")
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	return o.change(who, "admin.update", name, "password,disabled=false", func(d *store.Data) error {
		for i := range d.Admins {
			if a := &d.Admins[i]; a.Name == name {
				a.Hash, a.Disabled, a.MustChange = h, false, false
				return nil
			}
		}
		return notFound("admin.not_found")
	})
}

func (o Offline) change(who Actor, action, target, detail string, fn func(*store.Data) error) error {
	err := o.Store.Update(fn)
	if errors.Is(err, store.ErrReadOnly) {
		return conflict("store.readonly")
	}
	if err != nil {
		return err
	}
	_ = o.Store.Audit(store.AuditEntry{Actor: who.String(), Action: action, Target: target, Detail: detail})
	return nil
}

// checkNewAdmin checks a new administrator's name, password and role, and returns the password hash.
func checkNewAdmin(name, password, role string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", bad("user.name")
	}
	if role != "admin" && role != "viewer" {
		return "", bad("role.invalid")
	}
	if strings.EqualFold(password, name) {
		return "", bad("password.username")
	}
	return HashPassword(password)
}

// addAdmin adds an administrator unless the name is taken.
func addAdmin(d *store.Data, a store.Admin) error {
	for _, x := range d.Admins {
		if x.Name == a.Name {
			return conflict("admin.exists")
		}
	}
	d.Admins = append(d.Admins, a)
	return nil
}
