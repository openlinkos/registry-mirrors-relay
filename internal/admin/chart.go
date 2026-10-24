package admin

import (
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/status"
)

// 曲线的画布：横向拉伸到容器宽度（preserveAspectRatio="none"），线宽不随拉伸变化。
const (
	chartW = 720
	chartH = 120
)

// chartSeries 是一条曲线：数值和它的样式类（见 admin.css）。第一条曲线下面会填色。
type chartSeries struct {
	Values []float64
	Class  string
}

// chartSVG 画一张折线图。只有路径，没有文字和内联样式；刻度和图例写在图外的 HTML 里，
// 图本身用 aria-label 给出摘要。
func chartSVG(label string, series ...chartSeries) template.HTML {
	peak := 0.0
	n := 0
	for _, s := range series {
		n = max(n, len(s.Values))
		for _, v := range s.Values {
			peak = max(peak, v)
		}
	}
	var b strings.Builder
	b.WriteString(`<svg class="admin-chart" viewBox="0 0 ` + strconv.Itoa(chartW) + ` ` + strconv.Itoa(chartH) +
		`" preserveAspectRatio="none" role="img" aria-label="` + template.HTMLEscapeString(label) + `">`)
	for _, f := range []float64{0.25, 0.5, 0.75} {
		y := ftoa(chartH * f)
		b.WriteString(`<line class="admin-chart-grid" x1="0" x2="` + strconv.Itoa(chartW) + `" y1="` + y + `" y2="` + y + `" vector-effect="non-scaling-stroke"/>`)
	}
	if n >= 2 {
		for i, s := range series {
			pts := points(s.Values, n, peak)
			if i == 0 {
				b.WriteString(`<path class="admin-chart-area" d="M0,` + strconv.Itoa(chartH) + ` L` + pts + ` L` + strconv.Itoa(chartW) + `,` + strconv.Itoa(chartH) + ` Z"/>`)
			}
			b.WriteString(`<path class="` + s.Class + `" d="M` + pts + `" vector-effect="non-scaling-stroke"/>`)
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // 只有数字和转义过的说明
}

// points 把数值换成坐标："x,y L x,y …"。峰值占画布高度的 90%，全是 0 时贴着底边。
func points(vs []float64, n int, peak float64) string {
	var b strings.Builder
	for i, v := range vs {
		if i > 0 {
			b.WriteString(" L")
		}
		x := float64(i) * chartW / float64(n-1)
		y := float64(chartH) - 1
		if peak > 0 {
			y = chartH - v/peak*chartH*0.9 - 1
		}
		b.WriteString(ftoa(x) + "," + ftoa(y))
	}
	return b.String()
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// hourLabel 是一格的时间（UTC），给 title 用。
func hourLabel(start int64) string { return time.Unix(start, 0).UTC().Format("01-02 15:00 UTC") }

// hoursStrip 画近 24 小时的上游可用率：每小时一格，颜色按 status.Level，没有数据的格子是灰的。
// 和曲线一样只有类名，没有内联样式；每格的 title 写时间和检测次数，整条用 aria-label 概括。
func hoursStrip(hs []status.Hour, label string) template.HTML {
	var b strings.Builder
	b.WriteString(`<span class="admin-hours" role="img" aria-label="` + template.HTMLEscapeString(label+" "+hoursPct(hs)) + `">`)
	for _, h := range hs {
		class, title := "", hourLabel(h.Start)+" · —"
		if h.Total > 0 {
			class = " " + status.Level(float64(h.OK)/float64(h.Total))
			title = hourLabel(h.Start) + " · " + strconv.Itoa(h.OK) + "/" + strconv.Itoa(h.Total)
		}
		b.WriteString(`<span class="admin-hour` + class + `" title="` + template.HTMLEscapeString(title) + `"></span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String()) //nolint:gosec // 只有数字、类名和转义过的说明
}

// ratioStrip 画近 24 小时本站的请求成功率；请求太少的小时没有数值，格子是灰的。
func ratioStrip(rs []status.Ratio, label string) template.HTML {
	var b strings.Builder
	b.WriteString(`<span class="admin-hours" role="img" aria-label="` + template.HTMLEscapeString(label) + `">`)
	for _, r := range rs {
		class, title := "", hourLabel(r.Start)+" · —"
		if r.Value != nil {
			class = " " + status.Level(*r.Value)
			title = hourLabel(r.Start) + " · " + fmtPct(*r.Value, 1.0)
		}
		b.WriteString(`<span class="admin-hour` + class + `" title="` + template.HTMLEscapeString(title) + `"></span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String()) //nolint:gosec // 只有数字、类名和转义过的说明
}

// hoursPct 是近 24 小时的总可用率，没有数据时为 "—"。
func hoursPct(hs []status.Hour) string {
	ok, total := 0, 0
	for _, h := range hs {
		ok += h.OK
		total += h.Total
	}
	return fmtPct(ok, total)
}

// lastRatio 是最近一个有数值的小时的成功率，没有时为 -1。
func lastRatio(rs []status.Ratio) float64 {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Value != nil {
			return *rs[i].Value
		}
	}
	return -1
}
