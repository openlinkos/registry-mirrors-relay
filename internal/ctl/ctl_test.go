package ctl

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

type fakeAdmins struct {
	list     []ops.AdminView
	writable bool
	who      []ops.Actor
	created  []string
	updated  map[string]ops.AdminInput
	err      error
}

func (f *fakeAdmins) Admins() []ops.AdminView { return f.list }
func (f *fakeAdmins) Writable() bool          { return f.writable }

func (f *fakeAdmins) CreateAdmin(who ops.Actor, name, password, role string) error {
	f.who = append(f.who, who)
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, name+"/"+password+"/"+role)
	return nil
}

func (f *fakeAdmins) UpdateAdmin(who ops.Actor, name string, in ops.AdminInput) error {
	f.who = append(f.who, who)
	if f.err != nil {
		return f.err
	}
	if f.updated == nil {
		f.updated = map[string]ops.AdminInput{}
	}
	f.updated[name] = in
	return nil
}

func serve(t *testing.T, a Admins) *Client {
	t.Helper()
	srv := httptest.NewServer(Handler(a))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	var d net.Dialer
	return newClient(func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", u.Host) })
}

func TestHandler(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "alice")
	f := &fakeAdmins{writable: true, list: []ops.AdminView{{Name: "admin", Role: "admin"}}}
	c := serve(t, f)
	ctx := t.Context()

	var info Info
	if err := c.do(ctx, http.MethodGet, "/v1/info", nil, &info); err != nil || !info.Writable {
		t.Fatalf("info = %+v, %v", info, err)
	}
	list, err := c.Admins(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "admin" {
		t.Fatalf("admins = %+v, %v", list, err)
	}
	if err := c.CreateAdmin(ctx, "ops", "correct horse battery", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "ops/correct horse battery/admin" {
		t.Errorf("created = %v", f.created)
	}
	if err := c.SetPassword(ctx, "a b", "another long password"); err != nil {
		t.Fatal(err)
	}
	in, ok := f.updated["a b"]
	if !ok || in.Password == nil || *in.Password != "another long password" || in.Disabled == nil || *in.Disabled || in.Role != nil {
		t.Errorf("update = %+v", in)
	}
	for _, who := range f.who {
		if who.Kind != "cli" || who.Name != "alice" {
			t.Errorf("actor = %+v, want cli:alice", who)
		}
	}
}

func TestHandlerErrors(t *testing.T) {
	f := &fakeAdmins{err: &ops.Error{Status: http.StatusConflict, Key: "admin.exists"}}
	c := serve(t, f)
	err := c.CreateAdmin(t.Context(), "admin", "correct horse battery", "admin")
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict || e.Message == "" {
		t.Fatalf("err = %#v", err)
	}

	resp, err := c.hc.Post("http://relay/v1/admins", "application/json", strings.NewReader(`{"name":"x","bogus":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: status %d, want 400", resp.StatusCode)
	}
}

func TestActorName(t *testing.T) {
	f := &fakeAdmins{}
	h := Handler(f)
	for header, want := range map[string]string{"bob": "bob", "": "unknown", "evil\nname": "unknown", "uid:1000": "uid:1000"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/admins", strings.NewReader(`{"name":"x","password":"y"}`))
		r.Header.Set(UserHeader, header)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got := f.who[len(f.who)-1].Name; got != want {
			t.Errorf("header %q: actor %q, want %q", header, got, want)
		}
	}
}

func TestDialNotRunning(t *testing.T) {
	if _, _, err := Dial(t.Context(), t.TempDir()+"/missing.sock"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}

func TestPath(t *testing.T) {
	if got := Path("/var/lib/registry-mirrors-relay/state.db"); got != "/var/lib/registry-mirrors-relay/admin.sock" && got != `\var\lib\registry-mirrors-relay\admin.sock` {
		t.Errorf("Path = %q", got)
	}
}
