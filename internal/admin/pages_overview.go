package admin

import (
	"cmp"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/status"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

type overviewData struct {
	Live      map[string]string
	Warnings  []string
	Traffic   template.HTML
	Requests  template.HTML
	Peak      string // the busiest 10 minutes by traffic in the last 24 hours
	Day       stats.Minute
	Upstreams []ops.UpstreamView
	Status    status.Report // as on the usage page: overall status, this site's 24-hour success rate
	Interval  time.Duration // the upstream probe interval; 0 means no probing
	Images    []stats.Item
	Clients   []stats.Item
	Quota     *upstream.Quota
	Uptime    string
}

// liveValues are the numbers on the overview page that refresh automatically (already formatted).
func (h *Handler) liveValues(lang i18n.Lang) map[string]string {
	now := h.now()
	series := h.app.Stats.Series(now.Add(-59 * time.Minute))
	var hour, last5 stats.Minute
	for i, m := range series {
		hour.Requests += m.Requests
		hour.Bytes += m.Bytes
		hour.Status5xx += m.Status5xx
		hour.Limited += m.Limited
		if i >= len(series)-5 {
			last5.Requests += m.Requests
		}
	}
	ls := h.app.Limiter.Stats()
	ms := h.app.Runtime().Manifests.Stats()
	out := map[string]string{
		"rpm":        strconv.FormatFloat(float64(last5.Requests)/5, 'f', 1, 64),
		"bytes1h":    fmtBytes(hour.Bytes),
		"streams":    fmtNum(ls.BlobStreams),
		"inflight":   fmtNum(ls.Inflight),
		"err1h":      fmtPct(hour.Status5xx, hour.Requests),
		"hit":        fmtPct(ms.Hits, ms.Hits+ms.Misses+ms.Revalidations),
		"cacheBytes": fmtBytes(ms.Bytes),
		"limited1h":  fmtNum(hour.Limited),
		"quota":      "—",
	}
	if u := h.app.Runtime().Pool.Get(config.DefaultUpstream); u != nil {
		if q := u.Quota(); q != nil {
			out["quota"] = fmtNum(q.Remaining) + " / " + fmtNum(q.Limit)
		}
	}
	return out
}

// live is for the overview page's script: read-only, it returns liveValues.
func (h *Handler) live(c *ctx) error {
	b, _ := json.Marshal(h.liveValues(c.lang))
	c.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(b)
	return nil
}

func (h *Handler) overviewPage(c *ctx) error {
	rt := h.app.Runtime()
	d := overviewData{Live: h.liveValues(c.lang), Upstreams: h.ops.Upstreams(), Status: h.app.StatusReport()}
	d.Interval = time.Duration(d.Status.Interval) * time.Second

	// 24 hours, one point every 10 minutes
	series := h.app.Stats.Series(c.now.Add(-24*time.Hour + time.Minute))
	const bucket = 10
	var traffic, reqs, errs []float64
	var peak uint64
	for i := 0; i < len(series); i += bucket {
		var b stats.Minute
		for _, m := range series[i:min(i+bucket, len(series))] {
			b.Requests += m.Requests
			b.Bytes += m.Bytes
			b.Status4xx += m.Status4xx
			b.Status5xx += m.Status5xx
			d.Day.Requests += m.Requests
			d.Day.Bytes += m.Bytes
			d.Day.Status4xx += m.Status4xx
			d.Day.Status5xx += m.Status5xx
			d.Day.Limited += m.Limited
		}
		traffic = append(traffic, float64(b.Bytes))
		reqs = append(reqs, float64(b.Requests))
		errs = append(errs, float64(b.Status4xx+b.Status5xx))
		peak = max(peak, b.Bytes)
	}
	d.Peak = fmtBytes(peak)
	d.Traffic = chartSVG(tr(c.lang, "overview.trafficChart", "total", fmtBytes(d.Day.Bytes), "peak", d.Peak),
		chartSeries{traffic, "ol-series-1"})
	d.Requests = chartSVG(tr(c.lang, "overview.requestsChart", "total", fmtNum(d.Day.Requests), "errors", fmtNum(d.Day.Status4xx+d.Day.Status5xx)),
		chartSeries{reqs, "ol-series-1"}, chartSeries{errs, "ol-series-danger"})

	sum := h.app.Stats.Summarize(24*time.Hour, 10)
	d.Images, d.Clients = sum.Images, sum.Clients
	if u := rt.Pool.Get(config.DefaultUpstream); u != nil {
		d.Quota = u.Quota()
	}
	d.Uptime = fmtDur(c.lang, c.now.Sub(h.app.Started))

	// Things that need attention
	if st := h.app.LastReload(); st != nil {
		if st.Err != "" {
			d.Warnings = append(d.Warnings, tr(c.lang, "overview.reloadFailed", "error", cmp.Or(i18n.Localize(st.Cause, c.lang), st.Err)))
		}
		if len(st.Restart) > 0 {
			d.Warnings = append(d.Warnings, tr(c.lang, "overview.restartNeeded", "fields", strings.Join(st.Restart, ", ")))
		}
	}
	for _, u := range d.Upstreams {
		switch {
		case u.BreakerOpen:
			d.Warnings = append(d.Warnings, tr(c.lang, "overview.breakerOpen", "name", u.Name))
		case u.Health != nil && !u.Health.OK:
			d.Warnings = append(d.Warnings, tr(c.lang, "overview.probeFailed", "name", u.Name, "error", u.Health.Error))
		}
	}
	var suspended, drift []string
	for _, o := range rt.Overrides {
		switch o.State {
		case app.OverrideSuspended:
			suspended = append(suspended, o.Key)
		case app.OverrideDrift:
			drift = append(drift, o.Key)
		}
	}
	if len(suspended) > 0 {
		d.Warnings = append(d.Warnings, tr(c.lang, "overview.overridesSuspended", "n", len(suspended), "keys", strings.Join(suspended, ", ")))
	}
	if len(drift) > 0 {
		d.Warnings = append(d.Warnings, tr(c.lang, "overview.overridesDrift", "keys", strings.Join(drift, ", ")))
	}
	if n := h.app.AccessLogDropped(); n > 0 {
		d.Warnings = append(d.Warnings, tr(c.lang, "overview.logDropped", "n", fmtNum(n)))
	}
	return h.render(c, "overview", &view{Title: tr(c.lang, "nav.overview"), Nav: "overview", Data: d})
}
