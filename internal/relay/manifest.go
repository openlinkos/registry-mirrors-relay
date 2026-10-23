package relay

import (
	"net/http"
	"strconv"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/manifest"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// serveManifest 经缓存回答 manifest 请求。缓存处理不了时返回 false，由调用方按普通请求转发。
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, up *upstream.Upstream, t route.Target) bool {
	out := h.Manifests.Resolve(r.Context(), up, manifest.Request{
		Upstream: t.Upstream, Repo: t.Repo, Ref: t.Ref,
		Head: r.Method == http.MethodHead, Accept: r.Header.Values("Accept"),
	})
	e.Hops = out.Hops
	if out.Bypass {
		e.Cache = "bypass"
		return false
	}
	e.Cache = out.Cache
	switch {
	case out.NotFound:
		writeError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "镜像不存在或需要登录")
	case out.Err != nil:
		if manifest.IsTooLarge(out.Err) || manifest.IsMismatch(out.Err) {
			e.Error = out.Err.Error()
			h.Log.Warn("上游的 manifest 不能转发", "upstream", t.Upstream, "repo", t.Repo, "ref", t.Ref, "err", out.Err)
			writeError(w, r, http.StatusBadGateway, "MANIFEST_INVALID", out.Err.Error())
			return true
		}
		h.upstreamError(w, r, e, out.Err)
	case out.Status != 0:
		writeUpstreamError(w, r, out.Status, out.RetryAfter, out.BodyType, out.Body, t.Kind)
	case out.Manifest != nil:
		m := out.Manifest
		h.manifestHeaders(w.Header(), t, m.MediaType, m.Digest, int64(len(m.Body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(m.Body)
		}
	case out.Head != nil:
		h.manifestHeaders(w.Header(), t, out.Head.MediaType, out.Head.Digest, out.Head.Size)
		w.WriteHeader(http.StatusOK)
	}
	return true
}

func (h *Handler) manifestHeaders(hd http.Header, t route.Target, mediaType, digest string, size int64) {
	if mediaType != "" {
		hd.Set("Content-Type", mediaType)
	}
	hd.Set("Content-Length", strconv.FormatInt(size, 10))
	hd.Set("Docker-Content-Digest", digest)
	hd.Set("Docker-Distribution-Api-Version", "registry/2.0")
	hd.Set("Etag", `"`+digest+`"`)
	setSecurityHeaders(hd)
	switch {
	case t.Ref != digest:
		hd.Set("Cache-Control", "no-store") // 按 tag 请求：tag 会变
	case h.Config.Proxy.CDNCacheDigests:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}
