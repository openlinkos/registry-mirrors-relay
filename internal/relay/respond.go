package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// passHeaders 是成功响应里转给客户端的上游响应头。其他的（Set-Cookie、Alt-Svc、
// Docker-Ratelimit-Source 这类会泄露源站出口 IP 的头，以及 CDN 的各种头）一律不转。
var passHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
	"Docker-Content-Digest", "Docker-Distribution-Api-Version",
	"Etag", "Last-Modified", "Link", "Oci-Subject", "Oci-Filters-Applied",
}

// ratelimitHeaders 是上游的额度头。默认不转：它们描述的是整个服务共用的额度，用户会误以为是自己的。
var ratelimitHeaders = []string{"Ratelimit-Limit", "Ratelimit-Remaining", "X-Ratelimit-Limit", "X-Ratelimit-Remaining"}

func copyHeaders(dst, src http.Header, ratelimit bool) {
	for _, k := range passHeaders {
		if v := src.Values(k); len(v) > 0 {
			dst[k] = append([]string(nil), v...)
		}
	}
	if ratelimit {
		for _, k := range ratelimitHeaders {
			if v := src.Values(k); len(v) > 0 {
				dst[k] = append([]string(nil), v...)
			}
		}
	}
}

// setSecurityHeaders：镜像内容由镜像作者决定，和用法页同源，不能让浏览器把它当网页执行。
func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
}

type ociErrors struct {
	Errors []ociError `json:"errors"`
}

type ociError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	b, _ := json.Marshal(ociErrors{Errors: []ociError{{Code: code, Message: msg}}})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-store")
	h.Set("Docker-Distribution-Api-Version", "registry/2.0")
	setSecurityHeaders(h)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// maxErrorBody 是转给客户端的上游错误正文的上限。
const maxErrorBody = 64 << 10

// passError 把上游的 4xx、5xx 交给客户端，见 writeUpstreamError。
func (h *Handler) passError(w http.ResponseWriter, r *http.Request, resp *upstream.Response, t route.Target) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	writeUpstreamError(w, r, resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Content-Type"), body, t.Kind)
}

// writeUpstreamError：状态码不变；正文是 OCI 错误 JSON 就原样转，否则（例如 CDN 的 HTML 错误页）
// 换成我们生成的 OCI 错误。429、503 带上 Retry-After。
func writeUpstreamError(w http.ResponseWriter, r *http.Request, status int, retryAfter, ctype string, body []byte, kind route.Kind) {
	if retryAfter != "" && len(retryAfter) <= 64 && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
		w.Header().Set("Retry-After", retryAfter)
	}
	if len(body) > 0 && len(body) <= maxErrorBody && strings.Contains(ctype, "json") {
		var oe ociErrors
		if json.Unmarshal(body, &oe) == nil && len(oe.Errors) > 0 {
			hd := w.Header()
			hd.Set("Content-Type", "application/json")
			hd.Set("Content-Length", strconv.Itoa(len(body)))
			hd.Set("Cache-Control", "no-store")
			hd.Set("Docker-Distribution-Api-Version", "registry/2.0")
			setSecurityHeaders(hd)
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
	}
	code := "UNKNOWN"
	switch status {
	case http.StatusNotFound:
		code = notFoundCode(kind)
	case http.StatusForbidden:
		code = "DENIED"
	case http.StatusTooManyRequests:
		code = "TOOMANYREQUESTS"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		code = "UNAVAILABLE"
	}
	writeError(w, r, status, code, "上游返回 "+strconv.Itoa(status))
}

// recorder 记下状态码和写出的字节数，给访问日志用。Unwrap 让 http.ResponseController 能拿到原始的 ResponseWriter。
type recorder struct {
	http.ResponseWriter
	status int
	size   int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.size += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
