// Package relay 处理拉取请求：找入口、解析路径、转发给上游，按响应头策略把结果流式写回客户端。
// 镜像数据只经过内存里的一小块缓冲，不写盘。
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

// Deps 是 Handler 用到的组件。设置每个请求都从 App 的当前 Runtime 读，热重载后立即生效。
type Deps struct {
	App *app.App
	Log *slog.Logger
}

// Handler 处理拉取入口上的所有请求（/healthz、/readyz 由 server 包处理）。
type Handler struct {
	Deps
	bufs sync.Pool
}

// New 创建 Handler。转发缓冲的大小取启动时的配置（改了要重启）。
func New(d Deps) *Handler {
	size := int(d.App.Runtime().Config.Proxy.BufferSize)
	return &Handler{Deps: d, bufs: sync.Pool{New: func() any { b := make([]byte, size); return &b }}}
}

// manifestAccept 是客户端没带 Accept 时，向上游要 manifest 用的默认值。
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
			e.Status = 499 // 没来得及写响应，客户端已经走了
		}
		e.RespContentLength = rec.Header().Get("Content-Length")
		e.RespContentType = rec.Header().Get("Content-Type")
		h.App.Log(e)
		if p != nil {
			panic(p) // http.ErrAbortHandler：让客户端看到连接中断，而不是一个被截断却看似完整的响应
		}
	}()
	release, ok := h.App.Limiter.AcquireInflight()
	if !ok {
		e.Limited = true
		rec.Header().Set("Retry-After", "1")
		writeError(rec, r, http.StatusServiceUnavailable, "TOOMANYREQUESTS", "服务繁忙，稍后再试")
		return
	}
	defer release()
	h.serve(rec, r, e, rt, client)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, client netip.Addr) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "只支持拉取（GET、HEAD）")
		return
	}
	if r.ContentLength > 1024 || (r.ContentLength < 0 && len(r.TransferEncoding) > 0) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "请求体太大")
		return
	}
	ent := rt.Router.Entrance(route.ListenerFrom(r.Context()), r.Host)
	isV2 := r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/")
	if ent == nil {
		if isV2 {
			writeError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "这个域名没有配置入口")
		} else {
			http.NotFound(w, r)
		}
		return
	}
	e.Entrance = ent.Name
	if ok, reason := rt.IPRules.Decide(client, time.Now()); !ok {
		e.Error = reason
		writeError(w, r, http.StatusForbidden, "DENIED", "拒绝访问")
		return
	}
	if !isV2 {
		if !rt.Web.Serve(w, r) {
			http.NotFound(w, r)
		}
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
		writeError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "没有配置上游 "+t.Upstream)
		return
	}
	e.Upstream, e.Image, e.Kind, e.Ref = t.Upstream, t.Upstream+"/"+t.Repo, t.Kind.String(), t.Ref
	if ok, status, msg, rule := rt.Images.Decide(e.Image); !ok {
		e.Error = "image rule " + rule
		writeError(w, r, status, "DENIED", msg)
		return
	}

	// 限流：镜像层按并发流数，其余按令牌桶
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
				writeError(w, r, http.StatusServiceUnavailable, "TOOMANYREQUESTS", "服务繁忙，稍后再试")
			} else {
				writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "同时下载的镜像层太多")
			}
			return
		}
		defer rel()
	} else if ok, wait := h.App.Limiter.AllowManifest(key, client, r.Method == http.MethodHead, ov); !ok {
		e.Limited = true
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "请求太频繁，稍后再试")
		return
	}

	if t.Kind == route.KindManifest && rt.Manifests != nil && h.serveManifest(w, r, e, rt, up, t) {
		return
	}
	h.proxy(w, r, e, up, t)
}

// basicAuth 校验入口要求的登录，失败时写好响应并返回 nil。
func (h *Handler) basicAuth(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, client netip.Addr) *access.User {
	if ok, wait := h.App.Limiter.AuthAllowed(client); !ok {
		e.Limited = true
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeError(w, r, http.StatusTooManyRequests, "TOOMANYREQUESTS", "登录失败次数太多，稍后再试")
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
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "服务繁忙，稍后再试")
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

// challenge 要求客户端用 Basic 登录（docker login 会据此发送账号）。
func challenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Www-Authenticate", `Basic realm="registry-mirrors-relay", charset="UTF-8"`)
	writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "需要登录")
}

// requestHeaders 是转发给上游的请求头：只有 Accept，blob 再加 Range、If-Range。
// 不转发客户端的 Authorization、Cookie、X-Forwarded-*、User-Agent。
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
		// 白名单外的重定向：原样交给客户端，客户端会直连这个地址
		h.Log.Info("重定向不在白名单内，交给客户端", "upstream", up.Name, "location_host", hostOf(resp.PassLocation))
		h.App.Stats.UnlistedRedirect(up.Name, hostOf(resp.PassLocation))
		w.Header().Set("Location", resp.PassLocation)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	status := resp.StatusCode
	switch {
	case status == http.StatusUnauthorized:
		// 上游最终仍要求认证：通常是镜像不存在或是私有的。给客户端 401 会让它去要令牌，而我们不发令牌
		writeError(w, r, http.StatusNotFound, notFoundCode(t.Kind), "镜像不存在或需要登录")
		return
	case status >= 400:
		h.passError(w, r, resp, t)
		return
	case status >= 300 && status != http.StatusNotModified:
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "上游返回了意外的状态 "+strconv.Itoa(status))
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
		hd.Set("Cache-Control", "no-store") // tag 会变
	case h.App.Runtime().Config.Proxy.CDNCacheDigests:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			hd.Set("Cache-Control", cc)
		}
	}

	if resp.Probed {
		// HEAD 跟随重定向时用 GET Range: bytes=0-0 探测：按总长度回答
		size := probedSize(resp.Response)
		if size < 0 {
			writeError(w, r, http.StatusBadGateway, "UNKNOWN", "无法确定镜像层大小")
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
			h.Log.Warn("转发中断", "upstream", up.Name, "path", t.UpstreamPath(), "err", err)
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

// probedSize 从 Range: bytes=0-0 的响应里取总长度。上游忽略了 Range、直接返回 200 时用 Content-Length。
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
	errClientGone   = errors.New("客户端断开或写超时")
	errUpstreamIdle = errors.New("上游长时间没有数据")
)

// stream 把上游的正文写给客户端。没有总超时：读上游、写客户端各自有空闲超时，只要一直有进展就继续。
func (h *Handler) stream(w http.ResponseWriter, body io.Reader, cancel context.CancelFunc) error {
	rc := http.NewResponseController(w)
	_ = rc.Flush() // 先把响应头发出去
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
		cancel() // 取消上游请求，正在阻塞的 Read 会返回
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
		return // 客户端已经走了
	}
	var re *upstream.RedirectError
	var be *upstream.BlockedError
	var co *upstream.CircuitOpenError
	var ne net.Error
	switch {
	case errors.As(err, &co):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(co.RetryAfter.Seconds()+0.5))))
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "上游暂时不可用，稍后再试")
	case errors.As(err, &re):
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "上游重定向到了不允许的地址")
	case errors.As(err, &be):
		h.Log.Warn("拦下了到非公网地址的上游连接", "err", err)
		writeError(w, r, http.StatusBadGateway, "UNKNOWN", "上游地址不可用")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		w.Header().Set("Retry-After", "10")
		writeError(w, r, http.StatusGatewayTimeout, "UNAVAILABLE", "上游超时")
	default:
		h.Log.Warn("上游请求失败", "err", err)
		w.Header().Set("Retry-After", "10")
		writeError(w, r, http.StatusBadGateway, "UNAVAILABLE", "上游暂时不可用")
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
