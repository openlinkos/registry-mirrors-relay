// Package server 负责监听、TLS 和优雅退出。请求怎么处理由 Handler 决定。
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// Listener 是拉取入口以外的监听（管理后台、指标）。
type Listener struct {
	Name    string
	Addr    string
	Handler http.Handler
}

// Server 管理拉取入口和其他监听。
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	handler http.Handler
	extras  []Listener
	ready   atomic.Bool

	mu    sync.Mutex
	bound []string // 实际监听的地址，监听 :0 时能拿到端口
}

// New 创建 Server。handler 处理拉取入口上 /healthz、/readyz 以外的所有请求；extras 是其他监听，地址为空的忽略。
func New(cfg *config.Config, log *slog.Logger, handler http.Handler, extras ...Listener) *Server {
	s := &Server{cfg: cfg, log: log, handler: handler}
	for _, l := range extras {
		if l.Addr != "" {
			s.extras = append(s.extras, l)
		}
	}
	return s
}

// ServeHTTP 先处理探活，其余交给 handler。
//   - /healthz：进程在就返回 200，给 liveness 和外部监控用。
//   - /readyz：开始退出后返回 503，让 k8s 先把流量摘掉。它不检查上游：上游故障时 pod 不应全部变成未就绪。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		writeProbe(w, http.StatusOK)
		return
	case "/readyz":
		if s.ready.Load() {
			writeProbe(w, http.StatusOK)
		} else {
			writeProbe(w, http.StatusServiceUnavailable)
		}
		return
	}
	s.handler.ServeHTTP(w, r)
}

func writeProbe(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if code == http.StatusOK {
		_, _ = w.Write([]byte("ok\n"))
	} else {
		_, _ = w.Write([]byte("shutting down\n"))
	}
}

// Addrs 是需要监听的拉取入口地址：listen.http 和 listen.https。
func (s *Server) Addrs() []string {
	var out []string
	for _, a := range []string{s.cfg.Listen.HTTP, s.cfg.Listen.HTTPS} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Run 打开所有监听，直到 ctx 结束，然后优雅退出：
// 先让 /readyz 返回 503，停止接受新连接，已有的请求最多等 timeouts.shutdown_grace。
func (s *Server) Run(ctx context.Context) error {
	t := s.cfg.Timeouts
	tlsConf, acmeWrap, err := tlsSetup(s.cfg, s.log)
	if err != nil {
		return err
	}
	var servers []*http.Server
	var listeners []net.Listener
	var names []string
	var tlsOn []bool
	type plan struct {
		name, addr string
		h          http.Handler
		tls        bool
	}
	var plans []plan
	for _, addr := range s.Addrs() {
		p := plan{name: "拉取入口", addr: addr, h: s}
		switch {
		case addr == s.cfg.Listen.HTTPS && tlsConf != nil:
			p.name, p.tls = "拉取入口（HTTPS）", true
		case addr == s.cfg.Listen.HTTP && acmeWrap != nil:
			p.h = acmeWrap(s) // ACME 的 HTTP-01 验证，其余请求照常处理
		}
		plans = append(plans, p)
	}
	for _, l := range s.extras {
		plans = append(plans, plan{name: l.Name, addr: l.Addr, h: l.Handler})
	}
	for _, pl := range plans {
		addr := pl.addr
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("监听 %s：%w", addr, err)
		}
		listeners = append(listeners, ln)
		s.mu.Lock()
		s.bound = append(s.bound, ln.Addr().String())
		s.mu.Unlock()
		names = append(names, pl.name)
		tlsOn = append(tlsOn, pl.tls)
		var sc *tls.Config
		if pl.tls {
			sc = tlsConf.Clone()
		}
		servers = append(servers, &http.Server{
			Handler:           pl.h,
			TLSConfig:         sc,
			ReadHeaderTimeout: t.ReadHeader.D(),
			IdleTimeout:       t.Idle.D(),
			MaxHeaderBytes:    32 << 10,
			// 不设 WriteTimeout：大层可以下载很久，写空闲超时由转发逻辑按每次写设置
			ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		})
	}

	errc := make(chan error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server, ln net.Listener, name string, withTLS bool) {
			defer wg.Done()
			s.log.Info(name+"开始监听", "addr", ln.Addr().String())
			var err error
			if withTLS {
				err = srv.ServeTLS(ln, "", "") // 证书来自 TLSConfig；会自动启用 HTTP/2
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s：%w", ln.Addr(), err)
			}
		}(srv, listeners[i], names[i], tlsOn[i])
	}
	s.ready.Store(true)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	s.ready.Store(false)
	s.log.Info("开始退出，等待进行中的请求", "grace", t.ShutdownGrace.String())
	sctx, cancel := context.WithTimeout(context.Background(), t.ShutdownGrace.D())
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(sctx); err != nil {
			s.log.Warn("退出时仍有请求未完成，强制关闭", "err", err)
			_ = srv.Close()
		}
	}
	wg.Wait()
	return runErr
}

// Bound 返回实际监听的地址。
func (s *Server) Bound() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bound...)
}

// waitReady 只给测试用：等到 Run 已经开始接受请求。
func (s *Server) waitReady(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.ready.Load() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
