package web

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lan-dot-party/lanleecher/internal/macaddr"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

var funcs = template.FuncMap{
	"bytes":   formatBytes,
	"bps":     formatBps,
	"dt":      formatTime,
	"dtinput": formatTimeInput,
	"window":  formatScheduleWindow,
	"hm":      func(t time.Time) string { return t.Local().Format("15:04") },
	"ago":     formatAgo,
	"pct":     pct,
	"ratio":   ratio,
	"add":     func(a, b int64) int64 { return a + b },
	"addi":    func(a, b int) int { return a + b },
	"chart":   lineChart,
	"bars":    eventBars,
	"localmac": func(s string) bool {
		m, err := macaddr.Parse(s)
		return err == nil && m.IsLocal()
	},
	"dur": func(d time.Duration) string { return d.Round(time.Millisecond).String() },
	"deref": func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	},
	"seq":    func(vals ...int) []int { return vals },
	"lower":  strings.ToLower,
	"themes": func() []string { return store.Themes },
}

// formatBytes uses decimal units like the switch and ISP world (1 GB = 10^9 B).
func formatBytes(v int64) string {
	f := float64(v)
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	i := 0
	for math.Abs(f) >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", v)
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	if f >= 10 {
		return fmt.Sprintf("%.1f %s", f, units[i])
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func formatBps(v any) string {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	case int:
		f = float64(x)
	}
	units := []string{"bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"}
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func formatTimeInput(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02T15:04")
}

func formatAgo(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return formatTime(t)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// pct returns v as a percentage of max for SVG bar widths (0–100).
func pct(v, max int64) string {
	if max <= 0 || v <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.1f", math.Min(100, float64(v)*100/float64(max)))
}

// ratio formats up/down as "1 : 4.2" style upload share.
func ratio(down, up int64) string {
	if down == 0 && up == 0 {
		return "–"
	}
	if down == 0 {
		return "∞"
	}
	return fmt.Sprintf("%.2f", float64(up)/float64(down))
}

// lineChart renders download and upload of buckets as an SVG. Colors come
// from CSS classes, since the CSP forbids inline styles.
func lineChart(buckets []store.Bucket) template.HTML { return lineChartDetail(buckets, "", "") }

// lineChartDetail is lineChart with a click detail: a click (or a drag) into
// the chart loads detailURL&from=…&to=… (Unix seconds) into the element with
// the ID target.
func lineChartDetail(buckets []store.Bucket, detailURL, target string) template.HTML {
	if len(buckets) == 0 {
		return ""
	}
	const w, h, padL, padB, padT = 800.0, 220.0, 84.0, 24.0, 10.0
	// Fill gaps so idle periods drop to zero instead of interpolating.
	start, end := buckets[0].Start, buckets[len(buckets)-1].Start
	steps := int(end.Sub(start)/store.BucketSize) + 1
	if steps > 2000 {
		steps = 2000
	}
	down := make([]float64, steps)
	up := make([]float64, steps)
	for _, b := range buckets {
		i := int(b.Start.Sub(start) / store.BucketSize)
		if i >= 0 && i < steps {
			// bit/s average over the bucket; the bucket that is still running
			// is averaged over the time that has passed, not five minutes.
			secs := store.BucketSize.Seconds()
			if el := time.Since(b.Start).Seconds(); el < secs {
				secs = math.Max(el, 1)
			}
			down[i] = float64(b.BytesDown) * 8 / secs
			up[i] = float64(b.BytesUp) * 8 / secs
		}
	}
	maxV := 1.0
	for i := range down {
		maxV = math.Max(maxV, math.Max(down[i], up[i]))
	}
	x := func(i int) float64 {
		if steps == 1 {
			return padL + (w-padL)/2
		}
		return padL + float64(i)*(w-padL-4)/float64(steps-1)
	}
	y := func(v float64) float64 { return padT + (h-padT-padB)*(1-v/maxV) }
	path := func(vals []float64) string {
		var b strings.Builder
		for i, v := range vals {
			if i == 0 {
				fmt.Fprintf(&b, "M%.1f %.1f", x(i), y(v))
			} else {
				fmt.Fprintf(&b, " L%.1f %.1f", x(i), y(v))
			}
		}
		return b.String()
	}
	// The values go into data attributes so that app.js can show a tooltip
	// with time and rate under the mouse pointer.
	csv := func(vals []float64) string {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = strconv.FormatFloat(math.Round(v), 'f', -1, 64)
		}
		return strings.Join(parts, ",")
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="chart-wrap"><svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="Traffic"`+
		` data-start="%d" data-step="%.0f" data-x0="%.1f" data-x1="%.1f" data-y0="%.1f" data-y1="%.1f" data-max="%.0f"`+
		` data-down="%s" data-up="%s"`,
		w, h, start.Unix(), store.BucketSize.Seconds(), x(0), x(steps-1), y(0), y(maxV), maxV, csv(down), csv(up))
	if detailURL != "" {
		fmt.Fprintf(&b, ` data-detail="%s" data-detail-target="%s"`, template.HTMLEscapeString(detailURL),
			template.HTMLEscapeString(target))
	}
	b.WriteString(`>`)
	for _, f := range []float64{0, 0.5, 1} {
		yy := y(maxV * f)
		fmt.Fprintf(&b, `<line class="grid" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, padL, w, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%.0f" y="%.1f" text-anchor="end">%s</text>`, padL-6, yy+4,
			template.HTMLEscapeString(formatBps(maxV*f)))
	}
	fmt.Fprintf(&b, `<text class="axis" x="%.0f" y="%.0f">%s</text>`, padL, h-6, formatTime(start))
	fmt.Fprintf(&b, `<text class="axis" x="%.0f" y="%.0f" text-anchor="end">%s</text>`, w-4, h-6, formatTime(end))
	if steps > 1 {
		fmt.Fprintf(&b, `<path class="line-down" d="%s"/><path class="line-up" d="%s"/>`, path(down), path(up))
	} else {
		fmt.Fprintf(&b, `<circle class="dot-down" cx="%.1f" cy="%.1f" r="4"/><circle class="dot-up" cx="%.1f" cy="%.1f" r="4"/>`,
			x(0), y(down[0]), x(0), y(up[0]))
	}
	b.WriteString(`</svg></div>`)
	return template.HTML(b.String())
}

// eventBars renders download and upload per event as grouped bars.
func eventBars(rows []store.DeviceEventTotal) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	// Oldest event on the left.
	n := len(rows)
	if n > 20 {
		rows = rows[:20]
		n = 20
	}
	const h, padB, padT = 200.0, 40.0, 10.0
	slot := 96.0
	w := math.Max(640, slot*float64(n)+20)
	var maxV int64 = 1
	for _, r := range rows {
		maxV = max(maxV, r.BytesDown, r.BytesUp)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="Traffic per event">`, w, h)
	for i := 0; i < n; i++ {
		r := rows[n-1-i]
		x0 := 10 + float64(i)*slot + (slot-44)/2
		for j, v := range []int64{r.BytesDown, r.BytesUp} {
			bh := (h - padT - padB) * float64(v) / float64(maxV)
			class := "bar-down"
			if j == 1 {
				class = "bar-up"
			}
			fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="%.1f" width="20" height="%.1f"><title>%s</title></rect>`,
				class, x0+float64(j)*22, h-padB-bh, bh, template.HTMLEscapeString(formatBytes(v)))
		}
		name := r.Event.Name
		if len([]rune(name)) > 14 {
			name = string([]rune(name)[:13]) + "…"
		}
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x0+21, h-padB+14,
			template.HTMLEscapeString(name))
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x0+21, h-padB+28,
			r.Event.StartedAt.Local().Format("2006-01"))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
