package admin

// ---- Upstreams

func (h *Handler) upstreamsPage(c *ctx) error {
	return h.render(c, "upstreams", &view{Title: tr(c.lang, "nav.upstreams"), Nav: "upstreams", Data: h.ops.Upstreams()})
}

func (h *Handler) upstreamCheck(c *ctx) error {
	name := c.form.Get("upstream")
	hl, err := h.ops.CheckUpstream(c.r.Context(), c.actor(), name)
	if err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	if hl.OK {
		return c.done("/admin/upstreams", "upstream.ok", name)
	}
	return c.done("/admin/upstreams", "upstream.failed", name)
}

func (h *Handler) redirectHostAdd(c *ctx) error {
	host, err := h.ops.AddRedirectHost(c.actor(), c.form.Get("upstream"), c.form.Get("host"))
	if err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	return c.done("/admin/upstreams", "host.added", host)
}

func (h *Handler) redirectHostDelete(c *ctx) error {
	host := c.form.Get("host")
	if err := h.ops.DeleteRedirectHost(c.actor(), c.form.Get("upstream"), host); err != nil {
		return c.retry(err, h.upstreamsPage)
	}
	return c.done("/admin/upstreams", "host.deleted", host)
}
