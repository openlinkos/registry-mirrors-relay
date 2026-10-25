//go:build !windows

package ctl

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// shortDir returns a directory with a short path: t.TempDir() can exceed the socket path limit on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListen(t *testing.T) {
	path := filepath.Join(shortDir(t), "admin.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&fs.ModeSocket == 0 {
		t.Errorf("mode = %v, want a 0600 socket", fi.Mode())
	}
	srv := &http.Server{Handler: Handler(&fakeAdmins{writable: true, list: []ops.AdminView{{Name: "admin"}}})}
	go func() { _ = srv.Serve(ln) }()
	c, info, err := Dial(t.Context(), path)
	if err != nil || !info.Writable {
		t.Fatalf("Dial = %+v, %v", info, err)
	}
	if list, err := c.Admins(t.Context()); err != nil || len(list) != 1 {
		t.Errorf("Admins = %v, %v", list, err)
	}
	_ = srv.Shutdown(context.Background())
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind after close: %v", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "admin.sock")
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	if _, _, err := Dial(t.Context(), path); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Dial to a stale socket = %v, want ErrNotRunning", err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln.Close()
}

func TestListenRefusesOtherFiles(t *testing.T) {
	path := filepath.Join(shortDir(t), "admin.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Error("the file was changed")
	}
}

func TestAllowed(t *testing.T) {
	for _, c := range []struct {
		uid, self int
		want      bool
	}{{1000, 1000, true}, {0, 1000, true}, {1001, 1000, false}, {0, 0, true}, {65532, 0, false}} {
		if got := allowed(c.uid, c.self); got != c.want {
			t.Errorf("allowed(%d, %d) = %v", c.uid, c.self, got)
		}
	}
}
