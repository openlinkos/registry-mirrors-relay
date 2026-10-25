package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// tlsSetup prepares the HTTPS entrance according to tls.mode:
//   - files: reads the certificate and private key files, and swaps them automatically when the files
//     change (no restart needed after a certificate renewal);
//   - acme: obtains and renews certificates automatically with autocert, stored in tls.acme.cache_dir.
//     TLS-ALPN-01 is completed on the HTTPS entrance and HTTP-01 on the plain-text entrance (the returned
//     wrap goes around the plain-text entrance's handler; other requests are handled as usual).
//
// ACME only requests certificates for the domains hosts returns (tls.acme.hosts when nil), fetched afresh
// each time a new certificate is needed. When tls.mode is off it returns nil.
func tlsSetup(cfg *config.Config, log *slog.Logger, hosts func() []string) (conf *tls.Config, wrap func(http.Handler) http.Handler, err error) {
	switch cfg.TLS.Mode {
	case "files":
		r := &certReloader{certFile: cfg.TLS.CertFile, keyFile: cfg.TLS.KeyFile, log: log, now: time.Now}
		if err := r.load(); err != nil {
			return nil, nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, nil, nil
	case "acme":
		a := cfg.TLS.ACME
		if err := os.MkdirAll(a.CacheDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("tls.acme.cache_dir: %w", err)
		}
		if hosts == nil {
			hosts = func() []string { return a.Hosts }
		}
		m := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(a.CacheDir),
			// Called only when the cache has no certificate for this domain
			HostPolicy: func(ctx context.Context, host string) error {
				return autocert.HostWhitelist(hosts()...)(ctx, host)
			},
			Email: a.Email,
		}
		if a.DirectoryURL != "" {
			m.Client = &acme.Client{DirectoryURL: a.DirectoryURL}
		}
		conf := m.TLSConfig() // includes acme-tls/1, for TLS-ALPN-01
		conf.MinVersion = tls.VersionTLS12
		return conf, m.HTTPHandler, nil
	}
	return nil, nil, nil
}

// reloadEvery is the minimum interval between checks for changed certificate files (checked during the
// handshake, without a separate goroutine).
var reloadEvery = 30 * time.Second

// certReloader rereads the certificate files during the handshake as needed: it reloads them when their
// modification time or size changes, and keeps the old certificate if loading fails.
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string
	checked time.Time
}

func fileStamp(paths ...string) (string, error) {
	s := ""
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		s += fmt.Sprintf("%s:%d:%d;", p, fi.ModTime().UnixNano(), fi.Size())
	}
	return s, nil
}

func (r *certReloader) load() error {
	stamp, err := fileStamp(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("read certificate: %w", err)
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("read certificate: %w", err)
	}
	r.cert, r.stamp = &c, stamp
	return nil
}

func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= reloadEvery {
		r.checked = now
		if stamp, err := fileStamp(r.certFile, r.keyFile); err == nil && stamp != r.stamp {
			if err := r.load(); err != nil {
				r.log.Error("certificate files changed but could not be read, keeping the previous certificate", "err", err)
			} else {
				r.log.Info("certificate reloaded", "cert", r.certFile)
			}
		}
	}
	if r.cert == nil {
		return nil, errors.New("no certificate available")
	}
	return r.cert, nil
}
