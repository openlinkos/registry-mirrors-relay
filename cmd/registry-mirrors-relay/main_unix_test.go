//go:build !windows

package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ctl"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// TestAdminCommandRunning runs the admin subcommands against a running service, through its socket.
func TestAdminCommandRunning(t *testing.T) {
	dir, err := os.MkdirTemp("", "rr") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := "data_dir: " + dir + "\nlog: {access: {output: \"off\"}}\nproxy: {health_interval: 0s}\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(res, []byte(yaml), app.Options{ConfigPath: cfgPath, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	o := ops.New(a)
	var signedOut []string
	o.OnAdminChange = func(name string) { signedOut = append(signedOut, name) }
	ln, err := ctl.Listen(ctl.Path(res.Config.Admin.Store.Path))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: ctl.Handler(o)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// No administrator yet: password without a name creates admin
	code, out, errs := runCmd(t, "", "admin", "password", "--generate", "-config", cfgPath)
	pw := strings.TrimSpace(out)
	if code != 0 || len(pw) < 20 || !strings.Contains(errs, "Created the administrator admin") {
		t.Fatalf("first administrator: %d %q %s", code, out, errs)
	}
	admins := a.Store.Data().Admins
	if len(admins) != 1 || admins[0].MustChange || bcrypt.CompareHashAndPassword([]byte(admins[0].Hash), []byte(pw)) != nil {
		t.Fatalf("created through the running service: %+v", admins)
	}

	// The only administrator is the default; the change applies at once and signs it out
	if code, _, errs := runCmd(t, "another-long-password\n", "admin", "password", "-config", cfgPath); code != 0 || !strings.Contains(errs, "signed out") {
		t.Fatalf("password: %d %s", code, errs)
	}
	if h := a.Store.Data().Admins[0].Hash; bcrypt.CompareHashAndPassword([]byte(h), []byte("another-long-password")) != nil {
		t.Fatal("the new password is not in effect")
	}
	if len(signedOut) != 1 || signedOut[0] != "admin" {
		t.Fatalf("sessions signed out: %v", signedOut)
	}

	if code, _, _ := runCmd(t, "a-third-long-password\n", "admin", "create", "ops", "-role", "viewer", "-config", cfgPath); code != 0 {
		t.Fatalf("create: %d", code)
	}
	code, out, _ = runCmd(t, "", "admin", "list", "-config", cfgPath)
	if code != 0 || !strings.Contains(out, "admin  admin  active") || !strings.Contains(out, "ops  viewer  active") {
		t.Fatalf("list: %d %q", code, out)
	}
	// With several administrators the name is required
	if code, _, errs := runCmd(t, "x\n", "admin", "password", "-config", cfgPath); code != 2 || !strings.Contains(errs, "admin, ops") {
		t.Fatalf("several administrators: %d %s", code, errs)
	}
	if entries := a.Store.AuditLog(10); len(entries) == 0 || !strings.HasPrefix(entries[0].Actor, "cli:") {
		t.Fatalf("audit actor: %+v", entries)
	}
}
