// Package ctl is the local admin socket: a small HTTP API on a Unix socket next to the operational
// database, so that `registry-mirrors-relay admin …` works against a running service. The running
// service holds the database lock, so the command cannot open the database itself.
//
// Only processes of the service's own user (or root) can connect: the socket is mode 0600 in the
// data directory, and on Linux and macOS the peer's user id is checked on every connection.
package ctl

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// Path is where the socket lives for an operational database at storePath.
func Path(storePath string) string { return filepath.Join(filepath.Dir(storePath), "admin.sock") }

// UserHeader carries the name of the person running the command, for the audit log.
const UserHeader = "X-Relay-User"

// Info describes the running service.
type Info struct {
	Version  string `json:"version"`
	Writable bool   `json:"writable"`
}

// NewAdmin creates an administrator.
type NewAdmin struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// NewPassword sets an administrator's password.
type NewPassword struct {
	Password string `json:"password"`
}

// Admins is what the socket serves. *ops.Ops implements it.
type Admins interface {
	Admins() []ops.AdminView
	CreateAdmin(who ops.Actor, name, password, role string) error
	UpdateAdmin(who ops.Actor, name string, in ops.AdminInput) error
	Writable() bool
}

var userRe = regexp.MustCompile(`^[A-Za-z0-9._@:-]{1,64}$`)

// Handler serves the socket's API.
func Handler(a Admins) http.Handler {
	mux := http.NewServeMux()
	actor := func(r *http.Request) ops.Actor {
		name := r.Header.Get(UserHeader)
		if !userRe.MatchString(name) {
			name = "unknown"
		}
		return ops.Actor{Kind: "cli", Name: name}
	}
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Info{Version: version.Version, Writable: a.Writable()})
	})
	mux.HandleFunc("GET /v1/admins", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Admins())
	})
	mux.HandleFunc("POST /v1/admins", func(w http.ResponseWriter, r *http.Request) {
		var in NewAdmin
		if !readJSON(w, r, &in) {
			return
		}
		if in.Role == "" {
			in.Role = "admin"
		}
		if err := a.CreateAdmin(actor(r), in.Name, in.Password, in.Role); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("PUT /v1/admins/{name}/password", func(w http.ResponseWriter, r *http.Request) {
		var in NewPassword
		if !readJSON(w, r, &in) {
			return
		}
		enabled := false
		// Setting a password from the command line also re-enables the account: it is how a locked-out
		// operator gets back in.
		err := a.UpdateAdmin(actor(r), r.PathValue("name"), ops.AdminInput{Password: &in.Password, Disabled: &enabled})
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Error: "the request body is not valid JSON", Code: "request.json"})
		return false
	}
	return true
}

type wireError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, ops.Status(err), wireError{Error: err.Error(), Code: i18n.Code(err)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error is an error reported by the running service.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrNotRunning means that no service is listening on the socket.
var ErrNotRunning = errors.New("the service is not running")
