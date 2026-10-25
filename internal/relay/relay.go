// Package relay handles pull requests: it finds the entrance, parses the path, forwards the request to the
// upstream and streams the result back to the client according to the response header policy.
// Image data only passes through a small in-memory buffer and is never written to disk.
package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/access"
	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/limit"
	"github.com/openlinkos/registry-mirrors-relay/internal/manifest"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// Deps holds the components the Handler uses. Settings are read from the App's current Runtime on every
// request, so a hot reload takes effect immediately.
type Deps struct {
	App *app.App
	Log *slog.Logger
}

// Handler handles all requests on the pull entrance (/healthz and /readyz are handled by the server package).
type Handler struct {
	Deps
	bufs sync.Pool
}

// New creates a Handler. The forwarding buffer size comes from the configuration at startup (changing it
// requires a restart).
func New(d Deps) *Handler {
	size := int(d.App.Runtime().Config.Proxy.BufferSize)
	return &Handler{Deps: d, bufs: sync.Pool{New: func() any { b := make([]byte, size); return &b }}}
}

// manifestAccept is the default Accept used to request a manifest from the upstream when the client sends none.
const manifestAccept = manifest.DefaultAccept

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt := h.App.Runtime()
	rec := &recorder{ResponseWriter: w}
	client, remote := rt.IPs.ClientIP(r)
	_, port, _ := net.SplitHostPort(r.RemoteAddr)
	e := &accesslog.Entry{
		Time: start, RemoteIP: remote.String(), RemotePort: port, ClientIP: client.String(),
		Proto: r.Proto, Method: r.Method, Host: r.Host, URI: r.RequestURI, UserAgent: r.UserAgent(),
	}
	defer func() {
		p := recover()
		e.Duration = time.Since(start)
		e.Status, e.Size = rec.status, rec.size
		if e.Status == 0 {
			e.Status = 499 // the client left before a response was written
		}
		if e.Error == "" {
			e.Error = accesslog.NoteFrom(r.Context())
		}
		e.RespContentLength = rec.Header().Get("Content-Length")
		e.RespContentType = rec.Header().Get("Content-Type")
		h.App.Log(e)
		if p != nil {
			panic(p) // http.ErrAbortHandler: the client sees a broken connection, not a truncated response that looks complete
		}
	}()
	release, ok := h.App.Limiter.AcquireInflight()
	if !ok {
		e.Limited = true
		rec.Header().Set("Retry-After", "1")
		writeError(rec, r, http.StatusServiceUnavailable, "TOOMANYREQUESTS", "server busy, try again later")
		return
	}
	defer release()
	h.serve(rec, r, e, rt, client)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, client netip.Addr) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "only pull is supported (GET, HEAD)")
		return
	}
	if r.ContentLength > 1024 || (r.ContentLength < 0 && len(r.TransferEncoding) > 0) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "request body too large")
		return
	}
	ent := rt.Router.Entrance(r.Host)
	isV2 := r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/")
	if ent == nil {
		if isV2 {
			writeError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "no entrance configured for this host")
		} else {
			http.NotFound(w, r)
		}
		return
	}
	e.Entrance = ent.Name
	if ok, reason := rt.IPRules.Decide(client, time.Now()); !ok {
		e.Error = reason
		writeError(w, r, http.StatusForbidden, "DENIED", "access denied")
		return
	}
	if !isV2 {
		h.page(w, r, e, rt, client, ent)
		return
	}
	var user *access.User
	if ent.Auth == "basic" {
		if user = h.basicAuth(w, r, e, rt, client); user == nil {
			return
		}
		e.UserID = user.Name
	}
	t, perr := rt.Router.Parse(ent, r.URL)
	if perr != nil {
		writeError(w, r, perr.Status, perr.Code, perr.Message)
		return
	}
	e.Kind = t.Kind.String()
	if t.Kind == route.KindPing {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "{}")
		}
		return
	}
	up := rt.Pool.Get(t.Upstream)
	if up == nil {
		writeError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "upstream not configured: "+t.Upstream)
		return
	}
	e.Upstream, e.Image, e.Kind, e.Ref = t.Upstream, t.Upstream+"/"+t.Repo, t.Kind.String(), t.Ref
	if ok, status, msg, rule := rt.Images.Decide(e.Image); !ok {
		e.Error = "image rule " + rule
		h.App.Stats.RuleHit(rule)
		writeError(w, r, status, "DENIED", msg)
		return
	}

	// Rate limits: layers by number of concurrent streams, everything else by token bucket
	var ov limit.Override
	name := ""
	if user != nil {
		ov = limit.Override{ManifestRate: user.ManifestRPS, BlobStreams: user.BlobStreams}
		name = user.Name
	}
	key := limit.Key(name, client)
	if t.Kind == route.KindBlob {
		rel, ok, reason := h.App.Limiter.AcquireBlob(key, client, ov)
		if !ok {
			e.Limited = true
			w.Header().Set("Retry-After", "5")
			if reason == "global" {
				writeError(w, r, http.StatusServiceUnavailable, "TOOMANYREQUESTS", "server busy, try again later")
			} else {
				writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "too many concurrent layer downloads")
			}
			return
		}
		defer rel()
	} else if ok, wait := h.App.Limiter.AllowManifest(key, client, r.Method == http.MethodHead, ov); !ok {
		e.Limited = true
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "too many requests, try again later")
		return
	}

	if t.Kind == route.KindManifest && rt.Manifests != nil && h.serveManifest(w, r, e, rt, up, t) {
		return
	}
	h.proxy(w, r, e, up, t)
}

// page serves the usage page, site.json and status.json. When the entrance's page is public (the default)
// anyone can view them, and auth: basic only applies to pulls; with login, visitors must sign in first (with
// the same accounts as for pulls); with off, they are not served and only pulls remain.
func (h *Handler) page(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, client netip.Addr, ent *route.Entrance) {
	switch ent.Page {
	case "off":
		http.NotFound(w, r)
		return
	case "login":
		user := h.basicAuth(w, r, e, rt, client)
		if user == nil {
			return
		}
		e.UserID = user.Name
	}
	if !rt.Web.Serve(w, r, ent.Auth == "basic") {
		http.NotFound(w, r)
	}
}

// basicAuth checks the sign-in the entrance requires; on failure it writes the response and returns nil.
func (h *Handler) basicAuth(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, client netip.Addr) *access.User {
	if ok, wait := h.App.Limiter.AuthAllowed(client); !ok {
		e.Limited = true
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "too many failed sign-in attempts, try again later")
		return nil
	}
	name, pass, ok := r.BasicAuth()
	if !ok {
		challenge(w, r)
		return nil
	}
	u := rt.Users[name]
	good, err := h.App.Auth.Verify(u, pass)
	if errors.Is(err, access.ErrBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "server busy, try again later")
		return nil
	}
	if !good {
		h.App.Limiter.AuthFailed(client)
		e.Error = "login failed: " + name
		challenge(w, r)
		return nil
	}
	return u
}

// challenge asks the client to sign in with Basic auth (docker login sends the account in response).
func challenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Www-Authenticate", `Basic realm="registry-mirrors-relay", charset="UTF-8"`)
	writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

// requestHeaders returns the request headers forwarded to the upstream: only Accept, plus Range and If-Range
// for blobs. The client's Authorization, Cookie, X-Forwarded-* and User-Agent are not forwarded.
func requestHeaders(r *http.Request, k route.Kind) http.Header {
	h := http.Header{}
	accept := strings.Join(r.Header.Values("Accept"), ", ")
	if len(accept) > 4096 {
		accept = accept[:4096]
	}
	if accept == "" && k == route.KindManifest {
		accept = manifestAccept
	}
	if accept != "" {
		h.Set("Accept", accept)
	}
	if k == route.KindBlob {
		for _, name := range []string{"Range", "If-Range"} {
			if v := r.Header.Get(name); v != "" && len(v) <= 256 {
				h.Set(name, v)
			}
		}
	}
	return h
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, up *upstream.Upstream, t route.Target) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := up.Fetch(ctx, upstream.Request{
		Method: r.Method, Path: t.UpstreamPath(), Query: t.Query,
		Header: requestHeaders(r, t.Kind), Repo: t.Repo, Blob: t.Kind == route.KindBlob,
	})
	if err != nil {
		h.upstreamError(w, r, e, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.Hops = resp.Hops
	if resp.PassLocation != "" {
		// Redirect outside the allowlist: pass it to the client as is; the client connects to that address directly
		h.Log.Info("redirect not in allowlist, passed to client", "upstream", up.Name, "location_host", hostOf(resp.PassLocation))
		h.App.Stats.UnlistedRedirect(up.Name, hostOf(resp.PassLocation))
		w.Header().Set("Location", resp.PassLocation)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	status := resp.StatusCode
	switch {
	case status == http.StatusUnauthorized:
		// The upstream still requires authentication in the end: usually the image does not exist or is private.
		// A 401 would make the client ask for a token, and we do not issue tokens
		writeError(w, r, http.StatusNotFound, notFoundCode(t.Kind), "image not found or authentication required")
		return
	case status >= 400:
		h.passError(w, r, resp, t)
		return
	case status >= 300 && status != http.StatusNotModified:
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "upstream returned unexpected status "+strconv.Itoa(status))
		return
	}

	hd := w.Header()
	copyHeaders(hd, resp.Header, h.App.Runtime().Config.Proxy.ExposeUpstreamRatelimit)
	setSecurityHeaders(hd)
	isDigest := strings.Contains(t.Ref, ":")
	switch t.Kind {
	case route.KindBlob:
		hd.Set("Content-Type", "application/octet-stream")
		hd.Set("Docker-Content-Digest", t.Ref)
	case route.KindTags:
		hd.Del("Link")
		if link := resp.Header.Get("Link"); link != "" {
			if l := t.RewriteLink(link); l != "" {
				hd.Set("Link", l)
			}
		}
	default:
		hd.Del("Link")
	}
	switch {
	case !isDigest || t.Kind == route.KindTags:
		hd.Set("Cache-Control", "no-store") // tags can change
	case h.App.Runtime().Config.Proxy.CDNCacheDigests:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			hd.Set("Cache-Control", cc)
		}
	}

	if resp.Probed {
		// A HEAD that followed a redirect was probed with GET Range: bytes=0-0: answer with the total length
		size := probedSize(resp.Response)
		if size < 0 {
			writeError(w, r, http.StatusBadGateway, "UNKNOWN", "cannot determine layer size")
			return
		}
		hd.Del("Content-Range")
		hd.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead || status == http.StatusNotModified {
		return
	}
	if err := h.stream(w, resp.Body, cancel); err != nil {
		e.Error = err.Error()
		if !errors.Is(err, errClientGone) {
			h.Log.Warn("transfer interrupted", "upstream", up.Name, "path", t.UpstreamPath(), "err", err)
		}
		panic(http.ErrAbortHandler)
	}
}

func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// probedSize takes the total length from the response to Range: bytes=0-0. If the upstream ignored the Range
// and returned 200, it uses Content-Length.
func probedSize(resp *http.Response) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range") // bytes 0-0/12345
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				return n
			}
		}
		return -1
	}
	if resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	return -1
}

var (
	errClientGone   = errors.New("client disconnected or write timed out")
	errUpstreamIdle = errors.New("upstream sent no data for too long")
)

// stream writes the upstream body to the client. There is no overall timeout: reading from the upstream and
// writing to the client each have an idle timeout, and the transfer continues as long as it keeps progressing.
func (h *Handler) stream(w http.ResponseWriter, body io.Reader, cancel context.CancelFunc) error {
	rc := http.NewResponseController(w)
	_ = rc.Flush() // send the response headers first
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	bp := h.bufs.Get().(*[]byte)
	defer h.bufs.Put(bp)
	buf := *bp

	readIdle := h.App.Runtime().Config.Timeouts.UpstreamReadIdle.D()
	writeIdle := h.App.Runtime().Config.Timeouts.WriteIdle.D()
	var idle sync.Mutex
	timedOut := false
	timer := time.AfterFunc(readIdle, func() {
		idle.Lock()
		timedOut = true
		idle.Unlock()
		cancel() // cancel the upstream request so a blocked Read returns
	})
	defer timer.Stop()

	for {
		n, rerr := body.Read(buf)
		timer.Reset(readIdle)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeIdle))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return errClientGone
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			idle.Lock()
			to := timedOut
			idle.Unlock()
			if to {
				return errUpstreamIdle
			}
			return rerr
		}
	}
}

func (h *Handler) upstreamError(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, err error) {
	e.Error = err.Error()
	if r.Context().Err() != nil {
		return // the client has already left
	}
	var re *upstream.RedirectError
	var be *upstream.BlockedError
	var co *upstream.CircuitOpenError
	var ne net.Error
	switch {
	case errors.As(err, &co):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(co.RetryAfter.Seconds()+0.5))))
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "upstream temporarily unavailable, try again later")
	case errors.As(err, &re):
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "upstream redirected to a disallowed address")
	case errors.As(err, &be):
		h.Log.Warn("blocked upstream connection to a non-public address", "err", err)
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "upstream address unavailable")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		w.Header().Set("Retry-After", "10")
		writeError(w, r, http.StatusGatewayTimeout, "UNAVAILABLE", "upstream timed out")
	default:
		h.Log.Warn("upstream request failed", "err", err)
		w.Header().Set("Retry-After", "10")
		writeError(w, r, http.StatusBadGateway, "UNAVAILABLE", "upstream temporarily unavailable")
	}
}

func notFoundCode(k route.Kind) string {
	switch k {
	case route.KindManifest:
		return "MANIFEST_UNKNOWN"
	case route.KindBlob:
		return "BLOB_UNKNOWN"
	}
	return "NAME_UNKNOWN"
}
