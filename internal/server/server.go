// Package server takes care of listening, TLS and graceful shutdown. How requests are handled is up to the
// Handler.
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

// Listener is a listener other than the pull entrance (admin console, metrics).
type Listener struct {
	Name    string
	Addr    string
	Handler http.Handler
	// Listener is used instead of listening on Addr when set (the local admin socket). Run closes it.
	Listener net.Listener
}

// Server manages the pull entrance and the other listeners.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	handler http.Handler
	extras  []Listener
	ready   atomic.Bool
	// acmeHosts returns the ACME domains to allow right now; when nil, the ones in cfg are used
	acmeHosts func() []string

	mu    sync.Mutex
	bound []string // actual listen addresses, which give the port when listening on :0
}

// New creates a Server. handler handles all requests on the pull entrance other than /healthz and /readyz;
// extras are the other listeners, and those with an empty address are ignored.
func New(cfg *config.Config, log *slog.Logger, handler http.Handler, extras ...Listener) *Server {
	s := &Server{cfg: cfg, log: log, handler: handler}
	for _, l := range extras {
		if l.Addr != "" || l.Listener != nil {
			s.extras = append(s.extras, l)
		}
	}
	return s
}

// ACMEHosts makes ACME allow the domains f returns (tls.acme.hosts in the effective configuration), so
// changing entrances needs no restart. Call it before Run.
func (s *Server) ACMEHosts(f func() []string) { s.acmeHosts = f }

// ServeHTTP handles the probes first and passes everything else to handler.
//   - /healthz: returns 200 as long as the process is up, for liveness and external monitoring.
//   - /readyz: returns 503 once shutdown starts, so k8s removes the traffic first. It does not check
//     upstreams: an upstream outage should not make every pod unready.
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

// Addrs are the pull entrance addresses to listen on: listen.http and listen.https.
func (s *Server) Addrs() []string {
	var out []string
	for _, a := range []string{s.cfg.Listen.HTTP, s.cfg.Listen.HTTPS} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Run opens all listeners until ctx is done, then shuts down gracefully: /readyz returns 503 first, new
// connections stop being accepted, and in-flight requests get at most timeouts.shutdown_grace.
func (s *Server) Run(ctx context.Context) error {
	t := s.cfg.Timeouts
	tlsConf, acmeWrap, err := tlsSetup(s.cfg, s.log, s.acmeHosts)
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
		ln         net.Listener
	}
	var plans []plan
	for _, addr := range s.Addrs() {
		p := plan{name: "pull entrance", addr: addr, h: s}
		switch {
		case addr == s.cfg.Listen.HTTPS && tlsConf != nil:
			p.name, p.tls = "pull entrance (HTTPS)", true
		case addr == s.cfg.Listen.HTTP && acmeWrap != nil:
			p.h = acmeWrap(s) // ACME HTTP-01 validation; other requests are handled as usual
		}
		plans = append(plans, p)
	}
	for _, l := range s.extras {
		plans = append(plans, plan{name: l.Name, addr: l.Addr, h: l.Handler, ln: l.Listener})
	}
	for i, pl := range plans {
		addr := pl.addr
		ln, err := pl.ln, error(nil)
		if ln == nil {
			ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		}
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			for _, later := range plans[i+1:] {
				if later.ln != nil {
					_ = later.ln.Close()
				}
			}
			return fmt.Errorf("listen on %s: %w", addr, err)
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
			// No WriteTimeout: large layers can take a long time to download; the relay logic sets a write idle
			// timeout on every write
			ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		})
	}

	errc := make(chan error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server, ln net.Listener, name string, withTLS bool) {
			defer wg.Done()
			s.log.Info(name+" listening", "addr", ln.Addr().String())
			var err error
			if withTLS {
				err = srv.ServeTLS(ln, "", "") // certificates come from TLSConfig; HTTP/2 is enabled automatically
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", ln.Addr(), err)
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
	s.log.Info("shutting down, waiting for in-flight requests", "grace", t.ShutdownGrace.String())
	sctx, cancel := context.WithTimeout(context.Background(), t.ShutdownGrace.D())
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(sctx); err != nil {
			s.log.Warn("requests still unfinished at shutdown, forcing close", "err", err)
			_ = srv.Close()
		}
	}
	wg.Wait()
	return runErr
}

// Bound returns the actual listen addresses.
func (s *Server) Bound() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bound...)
}

// waitReady is for tests only: it waits until Run has started accepting requests.
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
