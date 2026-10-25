//go:build !windows

package ctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
)

// Listen opens the socket at path. A socket already there was left by a service that stopped (the
// caller holds the database lock, so no other service can be running); it is replaced. Anything
// else at path is an error and is left alone.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the old socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	// The data directory is 0700, so the moment before chmod exposes nothing.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &peerListener{Listener: ln, uid: os.Getuid()}, nil
}

// peerListener drops connections from other users, where the platform can tell.
type peerListener struct {
	net.Listener
	uid int
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err == nil && !allowed(uid, l.uid) {
			slog.Warn("admin socket: refused a connection from another user", "uid", uid)
			_ = c.Close()
			continue
		}
		if err != nil && !errors.Is(err, errors.ErrUnsupported) {
			slog.Warn("admin socket: cannot tell who connected", "err", err)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// allowed reports whether a peer with uid may use a socket owned by self: the same user, or root.
func allowed(uid, self int) bool { return uid == self || uid == 0 }
