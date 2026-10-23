package server

import (
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

// tlsSetup 按 tls.mode 准备 HTTPS 入口：
//   - files：读证书和私钥文件，文件变了自动换（证书续期后不用重启）；
//   - acme：用 autocert 自动申请和续期，证书存在 tls.acme.cache_dir。TLS-ALPN-01 在 HTTPS 入口上完成，
//     HTTP-01 在明文入口上完成（返回的 wrap 包在明文入口的 handler 外面，其余请求照常处理）。
//
// tls.mode 是 off 时返回 nil。
func tlsSetup(cfg *config.Config, log *slog.Logger) (conf *tls.Config, wrap func(http.Handler) http.Handler, err error) {
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
			return nil, nil, fmt.Errorf("tls.acme.cache_dir：%w", err)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(a.CacheDir),
			HostPolicy: autocert.HostWhitelist(a.Hosts...),
			Email:      a.Email,
		}
		if a.DirectoryURL != "" {
			m.Client = &acme.Client{DirectoryURL: a.DirectoryURL}
		}
		conf := m.TLSConfig() // 带上 acme-tls/1，用于 TLS-ALPN-01
		conf.MinVersion = tls.VersionTLS12
		return conf, m.HTTPHandler, nil
	}
	return nil, nil, nil
}

// reloadEvery 是检查证书文件有没有变的最短间隔（握手时检查，不另开协程）。
var reloadEvery = 30 * time.Second

// certReloader 在握手时按需重新读取证书文件：文件的修改时间或大小变了就重新加载，加载失败时继续用旧证书。
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
		return fmt.Errorf("读取证书：%w", err)
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("读取证书：%w", err)
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
				r.log.Error("证书文件变了，但读取失败，继续用原来的证书", "err", err)
			} else {
				r.log.Info("已重新加载证书", "cert", r.certFile)
			}
		}
	}
	if r.cert == nil {
		return nil, errors.New("没有可用的证书")
	}
	return r.cert, nil
}
