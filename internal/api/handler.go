package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Tdude/muntra/internal/store"
)

type Handler struct {
	pg           *store.Postgres
	allowedSites map[string]bool
}

func NewHandler(pg *store.Postgres, allowedSites map[string]bool) *Handler {
	return &Handler{pg: pg, allowedSites: allowedSites}
}

type rangeParams struct {
	site string
	from time.Time
	to   time.Time
}

func (h *Handler) parseRange(r *http.Request) (rangeParams, error) {
	q := r.URL.Query()
	site := q.Get("site")
	if site == "" || !h.allowedSites[site] {
		return rangeParams{}, errors.New("unknown site")
	}
	now := time.Now().UTC()
	defaultFrom := now.Add(-30 * 24 * time.Hour)
	from := parseMs(q.Get("from"), defaultFrom)
	to := parseMs(q.Get("to"), now)
	if !to.After(from) {
		from = defaultFrom
		to = now
	}
	return rangeParams{site: site, from: from, to: to}, nil
}

func parseMs(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return time.UnixMilli(n).UTC()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("api: encode failed", "err", err)
	}
}

type periodValue struct {
	Value uint64 `json:"value"`
	Prev  uint64 `json:"prev"`
}

type StatsResponse struct {
	Pageviews periodValue `json:"pageviews"`
	Visitors  periodValue `json:"visitors"`
	Visits    periodValue `json:"visits"`
}

type statsRow struct {
	Pageviews uint64
	Visitors  uint64
	Visits    uint64
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cur, err := h.queryStats(r.Context(), p.site, p.from, p.to)
	if err != nil {
		slog.Error("api: stats current failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	period := p.to.Sub(p.from)
	prev, err := h.queryStats(r.Context(), p.site, p.from.Add(-period), p.from)
	if err != nil {
		slog.Error("api: stats prev failed", "err", err)
		prev = statsRow{}
	}

	writeJSON(w, StatsResponse{
		Pageviews: periodValue{Value: cur.Pageviews, Prev: prev.Pageviews},
		Visitors:  periodValue{Value: cur.Visitors, Prev: prev.Visitors},
		Visits:    periodValue{Value: cur.Visits, Prev: prev.Visits},
	})
}

func (h *Handler) queryStats(ctx context.Context, site string, from, to time.Time) (statsRow, error) {
	const q = `
		SELECT
			COUNT(*) FILTER (WHERE event_name = 'pageview') AS pageviews,
			COUNT(DISTINCT visitor_hash) AS visitors,
			COUNT(DISTINCT session_hash) AS visits
		FROM raw_events
		WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
	`
	var row statsRow
	err := h.pg.Pool().QueryRow(ctx, q, site, from, to).Scan(&row.Pageviews, &row.Visitors, &row.Visits)
	if err != nil {
		return statsRow{}, fmt.Errorf("queryStats: %w", err)
	}
	return row, nil
}

type TimeseriesPoint struct {
	X         int64  `json:"x"`
	Pageviews uint64 `json:"pageviews"`
	Sessions  uint64 `json:"sessions"`
}

type TimeseriesResponse struct {
	Unit   string            `json:"unit"`
	Points []TimeseriesPoint `json:"points"`
}

var allowedUnits = map[string]string{
	"hour":  "hour",
	"day":   "day",
	"month": "month",
}

func (h *Handler) Timeseries(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	unit := r.URL.Query().Get("unit")
	bucket, ok := allowedUnits[unit]
	if !ok {
		unit = "day"
		bucket = "day"
	}
	// `bucket` is whitelisted via allowedUnits; safe to interpolate.
	q := fmt.Sprintf(`
		SELECT
			date_trunc('%s', created_at) AS bucket,
			COUNT(*) FILTER (WHERE event_name = 'pageview') AS pageviews,
			COUNT(DISTINCT session_hash) AS sessions
		FROM raw_events
		WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY bucket
		ORDER BY bucket
	`, bucket)

	rows, err := h.pg.Pool().Query(r.Context(), q, p.site, p.from, p.to)
	if err != nil {
		slog.Error("api: timeseries query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	points := []TimeseriesPoint{}
	for rows.Next() {
		var b time.Time
		var pv, ss uint64
		if err := rows.Scan(&b, &pv, &ss); err != nil {
			slog.Error("api: timeseries scan failed", "err", err)
			continue
		}
		points = append(points, TimeseriesPoint{X: b.UnixMilli(), Pageviews: pv, Sessions: ss})
	}
	if err := rows.Err(); err != nil {
		slog.Error("api: timeseries iter failed", "err", err)
	}
	writeJSON(w, TimeseriesResponse{Unit: unit, Points: points})
}

type BreakdownItem struct {
	X string `json:"x"`
	Y uint64 `json:"y"`
}

var breakdownFields = map[string]string{
	"url":      "url_path",
	"referrer": "referrer_domain",
	"country":  "country",
	"browser":  "ua_browser",
	"os":       "ua_os",
	"device":   "ua_device",
}

const (
	defaultBreakdownLimit = 100
	maxBreakdownLimit     = 10000
)

// parseBreakdownLimit parses the "limit" query param, falling back to
// defaultBreakdownLimit for anything empty, non-numeric, non-positive, or
// above maxBreakdownLimit.
//
// Learned the hard way in production: a caller that wants a complete
// per-URL view-count map (e.g. to build a "reads" badge per page) has to
// ask for a limit well above the default — say 5000, to cover every URL on
// a site. An earlier version of this handler capped accepted values at 500,
// so that request was silently rejected and fell back to the 100-item
// default. The query itself is ORDER BY y DESC LIMIT N — a top-N
// leaderboard — so anything below the cutoff wasn't undercounted, it was
// simply missing from the response: a 200 with a truncated array, no error.
// Any URL outside the top 100 by traffic (a brand-new page, or one that
// just isn't a top performer) silently reported as zero. If you're building
// a "give me every URL" consumer on top of /api/breakdown, make sure your
// requested limit is actually below this cap, or raise the cap to match.
func parseBreakdownLimit(raw string) int {
	if raw == "" {
		return defaultBreakdownLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxBreakdownLimit {
		return defaultBreakdownLimit
	}
	return n
}

func (h *Handler) Breakdown(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	col, ok := breakdownFields[r.URL.Query().Get("by")]
	if !ok {
		http.Error(w, "unknown 'by' value", http.StatusBadRequest)
		return
	}
	limit := parseBreakdownLimit(r.URL.Query().Get("limit"))
	// `col` is whitelisted via breakdownFields; safe to interpolate.
	q := fmt.Sprintf(`
		SELECT %s AS x, COUNT(*) AS y
		FROM raw_events
		WHERE site_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		  AND %s IS NOT NULL
		  AND %s <> ''
		GROUP BY %s
		ORDER BY y DESC
		LIMIT $4
	`, col, col, col, col)

	rows, err := h.pg.Pool().Query(r.Context(), q, p.site, p.from, p.to, limit)
	if err != nil {
		slog.Error("api: breakdown query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []BreakdownItem{}
	for rows.Next() {
		var x string
		var y uint64
		if err := rows.Scan(&x, &y); err != nil {
			slog.Error("api: breakdown scan failed", "err", err)
			continue
		}
		items = append(items, BreakdownItem{X: x, Y: y})
	}
	writeJSON(w, items)
}

type LiveResponse struct {
	Active uint64 `json:"active"`
}

func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	site := r.URL.Query().Get("site")
	if site == "" || !h.allowedSites[site] {
		http.Error(w, "unknown site", http.StatusBadRequest)
		return
	}
	const q = `
		SELECT COUNT(DISTINCT visitor_hash)
		FROM raw_events
		WHERE site_id = $1 AND created_at >= NOW() - INTERVAL '5 minutes'
	`
	var active uint64
	if err := h.pg.Pool().QueryRow(r.Context(), q, site).Scan(&active); err != nil {
		slog.Error("api: live failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, LiveResponse{Active: active})
}
