package relay

import (
	"net/http"
	"strconv"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/manifest"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// serveManifest answers a manifest request through the cache. It returns false when the cache cannot handle
// the request, and the caller forwards it as an ordinary request.
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, e *accesslog.Entry, rt *app.Runtime, up *upstream.Upstream, t route.Target) bool {
	out := rt.Manifests.Resolve(r.Context(), up, manifest.Request{
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
		writeError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "image not found or authentication required")
	case out.Err != nil:
		if manifest.IsTooLarge(out.Err) || manifest.IsMismatch(out.Err) {
			e.Error = out.Err.Error()
			h.Log.Warn("upstream manifest cannot be relayed", "upstream", t.Upstream, "repo", t.Repo, "ref", t.Ref, "err", out.Err)
			writeError(w, r, http.StatusBadGateway, "MANIFEST_INVALID", out.Err.Error())
			return true
		}
		h.upstreamError(w, r, e, out.Err)
	case out.Status != 0:
		writeUpstreamError(w, r, out.Status, out.RetryAfter, out.BodyType, out.Body, t.Kind)
	case out.Manifest != nil:
		m := out.Manifest
		manifestHeaders(w.Header(), t, m.MediaType, m.Digest, int64(len(m.Body)), rt.Config.Proxy.CDNCacheDigests)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(m.Body)
		}
	case out.Head != nil:
		manifestHeaders(w.Header(), t, out.Head.MediaType, out.Head.Digest, out.Head.Size, rt.Config.Proxy.CDNCacheDigests)
		w.WriteHeader(http.StatusOK)
	}
	return true
}

func manifestHeaders(hd http.Header, t route.Target, mediaType, digest string, size int64, cdnCacheDigests bool) {
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
		hd.Set("Cache-Control", "no-store") // requested by tag: tags can change
	case cdnCacheDigests:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}
