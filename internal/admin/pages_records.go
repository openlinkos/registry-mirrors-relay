package admin

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

const perPage = 100

type pagerView struct {
	Page       int // 从 1 开始
	Prev, Next string
}

// pager 生成上一页、下一页的链接，保留当前的查询参数。
func pager(c *ctx, page int, more bool) pagerView {
	link := func(p int) string {
		q := c.r.URL.Query()
		if p == 0 {
			q.Del("page")
		} else {
			q.Set("page", strconv.Itoa(p))
		}
		if s := q.Encode(); s != "" {
			return c.r.URL.Path + "?" + s
		}
		return c.r.URL.Path
	}
	pv := pagerView{Page: page + 1}
	if page > 0 {
		pv.Prev = link(page - 1)
	}
	if more {
		pv.Next = link(page + 1)
	}
	return pv
}

func pageParam(c *ctx, maxPage int) int {
	p, _ := strconv.Atoi(c.r.URL.Query().Get("page"))
	return min(max(p, 0), maxPage)
}

type requestsData struct {
	Rows      []stats.Request
	Filter    url.Values
	Upstreams []string
	Pager     pagerView
	Filtered  bool
}

func (h *Handler) requestsPage(c *ctx) error {
	q := c.r.URL.Query()
	f := stats.Filter{ClientIP: strings.TrimSpace(q.Get("client_ip")), Upstream: q.Get("upstream"),
		Image: strings.TrimSpace(q.Get("image")), Query: strings.TrimSpace(q.Get("q"))}
	f.Status, _ = strconv.Atoi(q.Get("status"))
	page := pageParam(c, 49)
	rows := h.app.Stats.Recent(f, (page+1)*perPage+1)
	more := len(rows) > (page+1)*perPage
	rows = rows[min(page*perPage, len(rows)):min((page+1)*perPage, len(rows))]
	d := requestsData{Rows: rows, Filter: q, Upstreams: h.app.Runtime().Pool.Names(), Pager: pager(c, page, more),
		Filtered: f != (stats.Filter{})}
	return h.render(c, "requests", &view{Title: tr(c.lang, "nav.requests"), Nav: "requests", Data: d})
}

type repeatView struct {
	Client, Image, Digest string
	Count                 uint64
	Bytes                 int64
}

type abuseData struct {
	Hours   int
	Periods []int
	Clients []stats.Item
	Repeats []repeatView
	Totals  stats.Minute
}

func (h *Handler) abusePage(c *ctx) error {
	hours, _ := strconv.Atoi(c.r.URL.Query().Get("hours"))
	switch hours {
	case 1, 24, 168:
	default:
		hours = 24
	}
	sum := h.app.Stats.Summarize(time.Duration(hours)*time.Hour, 50)
	d := abuseData{Hours: hours, Periods: []int{1, 24, 168}, Clients: sum.Clients, Totals: sum.Totals}
	for _, it := range sum.Repeats {
		if it.Count < 2 {
			continue
		}
		client, rest, _ := strings.Cut(it.Key, " ")
		img, dg, _ := strings.Cut(rest, "@")
		d.Repeats = append(d.Repeats, repeatView{Client: client, Image: img, Digest: dg, Count: it.Count, Bytes: it.Bytes})
	}
	return h.render(c, "abuse", &view{Title: tr(c.lang, "nav.abuse"), Nav: "abuse", Data: d})
}

type auditData struct {
	Rows  []store.AuditEntry
	Pager pagerView
}

func (h *Handler) auditPage(c *ctx) error {
	page := pageParam(c, 99)
	rows := h.app.Store.AuditLog((page+1)*perPage + 1)
	more := len(rows) > (page+1)*perPage
	rows = rows[min(page*perPage, len(rows)):min((page+1)*perPage, len(rows))]
	return h.render(c, "audit", &view{Title: tr(c.lang, "nav.audit"), Nav: "audit", Data: auditData{Rows: rows, Pager: pager(c, page, more)}})
}
