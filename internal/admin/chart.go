package admin

import (
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/status"
)

// The chart canvas: stretched horizontally to the container width (preserveAspectRatio="none"); the
// stroke width does not change with the stretching.
const (
	chartW = 720
	chartH = 120
)

// chartSeries is one line: its values and its style class (see admin.css). The area under the first line
// is filled.
type chartSeries struct {
	Values []float64
	Class  string
}

// chartSVG draws a line chart. It contains only paths, no text and no inline styles; the scale and the
// legend are written in the HTML outside the chart, and the chart itself gives a summary in aria-label.
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
	return template.HTML(b.String()) //nolint:gosec // only numbers and the escaped label
}

// points turns the values into coordinates: "x,y L x,y …". The peak takes 90% of the canvas height;
// when every value is 0 the line runs along the bottom edge.
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

// hourLabel is the time of one cell (UTC), used in the title.
func hourLabel(start int64) string { return time.Unix(start, 0).UTC().Format("01-02 15:00 UTC") }

// hoursStrip draws the upstream availability over the last 24 hours: one cell per hour, coloured by
// status.Level; cells without data are grey. As with the chart there are only class names, no inline
// styles; each cell's title gives the time and the number of checks, and aria-label sums up the strip.
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
	return template.HTML(b.String()) //nolint:gosec // only numbers, class names and the escaped label
}

// ratioStrip draws this site's request success rate over the last 24 hours; hours with too few requests
// have no value and their cells are grey.
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
	return template.HTML(b.String()) //nolint:gosec // only numbers, class names and the escaped label
}

// hoursPct is the overall availability over the last 24 hours, or "—" when there is no data.
func hoursPct(hs []status.Hour) string {
	ok, total := 0, 0
	for _, h := range hs {
		ok += h.OK
		total += h.Total
	}
	return fmtPct(ok, total)
}

// lastRatio is the success rate of the most recent hour that has a value, or -1 when there is none.
func lastRatio(rs []status.Ratio) float64 {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Value != nil {
			return *rs[i].Value
		}
	}
	return -1
}
