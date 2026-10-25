package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// Client talks to a running service over its socket.
type Client struct {
	hc   *http.Client
	user string
}

// Dial connects to the service listening at path. It returns ErrNotRunning when nobody listens
// there, so the caller can open the database itself.
func Dial(ctx context.Context, path string) (*Client, Info, error) {
	if runtime.GOOS == "windows" {
		return nil, Info{}, ErrNotRunning // the service does not open the socket on Windows
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, Info{}, ErrNotRunning
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c := newClient(func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", path) })
	var info Info
	if err := c.do(ctx, http.MethodGet, "/v1/info", nil, &info); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
			return nil, Info{}, ErrNotRunning // a socket left behind by a service that stopped
		}
		return nil, Info{}, fmt.Errorf("connect to the service at %s: %w", path, err)
	}
	return c, info, nil
}

func newClient(dial func(context.Context) (net.Conn, error)) *Client {
	return &Client{
		hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx)
			}},
		},
		user: CurrentUser(),
	}
}

// CurrentUser names the person running the command, for the audit log.
func CurrentUser() string {
	for _, k := range []string{"SUDO_USER", "USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if runtime.GOOS != "windows" {
		return "uid:" + strconv.Itoa(os.Getuid())
	}
	return "unknown"
}

// Admins lists the administrators.
func (c *Client) Admins(ctx context.Context) ([]Admin, error) {
	var out []Admin
	err := c.do(ctx, http.MethodGet, "/v1/admins", nil, &out)
	return out, err
}

// CreateAdmin creates an administrator.
func (c *Client) CreateAdmin(ctx context.Context, name, password, role string) error {
	return c.do(ctx, http.MethodPost, "/v1/admins", NewAdmin{Name: name, Password: password, Role: role}, nil)
}

// SetPassword sets an administrator's password, re-enables the account and signs out its sessions.
func (c *Client) SetPassword(ctx context.Context, name, password string) error {
	return c.do(ctx, http.MethodPut, "/v1/admins/"+url.PathEscape(name)+"/password", NewPassword{Password: password}, nil)
}

// Admin is an administrator as the socket lists it.
type Admin struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Disabled   bool      `json:"disabled"`
	MustChange bool      `json:"must_change"`
	Created    time.Time `json:"created"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(UserHeader, c.user)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var we wireError
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&we) != nil || we.Error == "" {
			we.Error = resp.Status
		}
		return &Error{Status: resp.StatusCode, Code: we.Code, Message: we.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
