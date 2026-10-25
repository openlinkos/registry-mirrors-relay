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

// passHeaders are the upstream response headers passed to the client in a successful response. All others
// (Set-Cookie, Alt-Svc, headers such as Docker-Ratelimit-Source that leak the origin's egress IP, and the
// various CDN headers) are never passed on.
var passHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
	"Docker-Content-Digest", "Docker-Distribution-Api-Version",
	"Etag", "Last-Modified", "Link", "Oci-Subject", "Oci-Filters-Applied",
}

// ratelimitHeaders are the upstream's quota headers. They are not passed on by default: they describe the
// quota shared by the whole service, and users would mistake it for their own.
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

// setSecurityHeaders: image content is chosen by the image author and shares an origin with the usage page,
// so browsers must not run it as a web page.
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

// maxErrorBody is the size limit for an upstream error body passed to the client.
const maxErrorBody = 64 << 10

// passError passes an upstream 4xx or 5xx to the client; see writeUpstreamError.
func (h *Handler) passError(w http.ResponseWriter, r *http.Request, resp *upstream.Response, t route.Target) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	writeUpstreamError(w, r, resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Content-Type"), body, t.Kind)
}

// writeUpstreamError keeps the status code. An OCI error JSON body is passed on as is; anything else (a
// CDN's HTML error page, for example) is replaced with an OCI error we generate. 429 and 503 carry Retry-After.
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
	writeError(w, r, status, code, "upstream returned "+strconv.Itoa(status))
}

// recorder records the status code and the number of bytes written, for the access log. Unwrap lets
// http.ResponseController reach the original ResponseWriter.
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
