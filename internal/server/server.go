// Package server 负责监听、TLS 和优雅退出。请求怎么处理由 Handler 决定。
package server

import (
	"context"
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

// Server 管理拉取入口的各个监听。
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	handler http.Handler
	ready   atomic.Bool

	mu    sync.Mutex
	bound []string // 实际监听的地址，监听 :0 时能拿到端口
}

// New 创建 Server。handler 处理 /healthz、/readyz 以外的所有请求。
func New(cfg *config.Config, log *slog.Logger, handler http.Handler) *Server {
	return &Server{cfg: cfg, log: log, handler: handler}
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

// Addrs 是需要监听的拉取入口地址：listen.http、listen.https，以及按端口区分的入口。
func (s *Server) Addrs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	add(s.cfg.Listen.HTTP)
	add(s.cfg.Listen.HTTPS)
	for _, e := range s.cfg.Entrances {
		add(e.Listen)
	}
	return out
}

// Run 打开所有监听，直到 ctx 结束，然后优雅退出：
// 先让 /readyz 返回 503，停止接受新连接，已有的请求最多等 timeouts.shutdown_grace。
func (s *Server) Run(ctx context.Context) error {
	t := s.cfg.Timeouts
	var servers []*http.Server
	var listeners []net.Listener
	for _, addr := range s.Addrs() {
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
		servers = append(servers, &http.Server{
			Handler:           s,
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
		go func(srv *http.Server, ln net.Listener) {
			defer wg.Done()
			s.log.Info("拉取入口开始监听", "addr", ln.Addr().String())
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s：%w", ln.Addr(), err)
			}
		}(srv, listeners[i])
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
