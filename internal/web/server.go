package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mk/daywatch/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Section describes one record type's list page.
type Section struct {
	Slug        string // URL path segment
	Type        string // record type discriminator
	Title       string
	Icon        string
	StatusLabel string
	// Columns rendered in the list table: header -> function of record
	Columns []Column
	// GroupLabelExpr is the SQL expression used to label group aggregates.
	GroupLabelExpr string
	GroupTitle     string
	// Chart legend labels for the ok/warn/err status classes.
	OKLabel   string
	WarnLabel string
	ErrLabel  string
	// SlowTitle enables a "slowest by avg duration" aggregate card.
	SlowTitle string
	// HasDuration enables the newest/slowest sort toggle on the list.
	HasDuration bool
}

type Column struct {
	Header string
	Value  func(r store.Record) template.HTML
}

type Server struct {
	store       *store.Store
	log         *slog.Logger
	tmpl        *template.Template
	sections    []Section
	bySlug      map[string]*Section
	hub         *Hub
	auth        AuthConfig
	alertTester AlertTester
}

// Hub returns the live-update hub; the ingest pipeline calls Notify on it.
func (s *Server) Hub() *Hub { return s.hub }

func New(st *store.Store, log *slog.Logger, auth AuthConfig) (*Server, error) {
	s := &Server{store: st, log: log, bySlug: map[string]*Section{}, hub: NewHub(), auth: auth}
	s.sections = buildSections()
	for i := range s.sections {
		s.bySlug[s.sections[i].Slug] = &s.sections[i]
	}

	funcs := template.FuncMap{
		"ms": func(v any) string {
			var f float64
			switch t := v.(type) {
			case int64:
				f = float64(t)
			case float64:
				f = t
			}
			// Durations arrive in microseconds from Nightwatch sensors.
			switch {
			case f >= 1_000_000:
				return fmt.Sprintf("%.2fs", f/1_000_000)
			case f >= 1_000:
				return fmt.Sprintf("%.1fms", f/1_000)
			default:
				return fmt.Sprintf("%.0fµs", f)
			}
		},
		"timefmt": func(t time.Time) string { return t.Local().Format("Jan 02 15:04:05") },
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"json": func(v any) string {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return "{}"
			}
			return string(b)
		},
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
		"icon": icon,
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /section/{slug}", s.handleSection)
	mux.HandleFunc("GET /record/{id}", s.handleRecord)
	mux.HandleFunc("GET /trace/{trace}", s.handleTrace)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("GET /alerts", s.handleAlerts)
	mux.HandleFunc("POST /alerts", s.handleAlertCreate)
	mux.HandleFunc("POST /alerts/{id}/toggle", s.handleAlertToggle)
	mux.HandleFunc("POST /alerts/{id}/delete", s.handleAlertDelete)
	mux.HandleFunc("POST /alerts/{id}/test", s.handleAlertTest)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	return s.requireAuth(mux)
}

// baseData is embedded in every page render.
type baseData struct {
	Sections    []Section
	ActiveSlug  string
	Range       string
	RangeLabel  string
	QueryString string
	AuthEnabled bool
}

func (s *Server) base(r *http.Request, active string) (baseData, time.Time) {
	rng := r.URL.Query().Get("range")
	var since time.Time
	var label string
	switch rng {
	case "1h":
		since, label = time.Now().Add(-time.Hour), "last hour"
	case "7d":
		since, label = time.Now().Add(-7*24*time.Hour), "last 7 days"
	case "30d":
		since, label = time.Now().Add(-30*24*time.Hour), "last 30 days"
	default:
		rng, since, label = "24h", time.Now().Add(-24*time.Hour), "last 24 hours"
	}
	return baseData{
		Sections:    s.sections,
		ActiveSlug:  active,
		Range:       rng,
		RangeLabel:  label,
		AuthEnabled: s.auth.Enabled(),
	}, since
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render failed", "template", name, "error", err)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	base, since := s.base(r, "")
	ctx := r.Context()

	counts, err := s.store.CountsByType(ctx, since)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	routes, err := s.store.RequestStats(ctx, since, 15)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	slowQueries, err := s.store.GroupStats(ctx, "query", "data->>'sql'", "total", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	exceptions, err := s.store.GroupStats(ctx, "exception", "concat(data->>'class', ': ', data->>'message')", "count", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	bm := bucketMinutes(time.Since(since))
	timeline, err := s.store.TimelineByClass(ctx, "request", since, time.Time{}, bm)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	recent, err := s.store.List(ctx, store.ListFilter{Since: since, Limit: 15})
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	var total int64
	countMap := map[string]int64{}
	for _, c := range counts {
		countMap[c.Type] = c.Count
		total += c.Count
	}

	s.render(w, "dashboard.html", map[string]any{
		"Base":        base,
		"Counts":      countMap,
		"Total":       total,
		"Routes":      routes,
		"SlowQueries": slowQueries,
		"Exceptions":  exceptions,
		"Chart": chartJSON(timeline, bm, chartOpts{
			DrillURL:    "/section/requests",
			DrillParams: "range=" + base.Range,
			OKLabel:     "2xx/3xx",
			WarnLabel:   "4xx",
			ErrLabel:    "5xx",
		}),
		"Recent": recent,
	})
}

func (s *Server) handleSection(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.bySlug[r.PathValue("slug")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	base, since := s.base(r, sec.Slug)
	ctx := r.Context()
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 50

	// A clicked chart bar drills into its exact time window.
	var until time.Time
	var window string
	if from, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil && from > 0 {
		if to, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil && to > from {
			since = time.Unix(from, 0)
			until = time.Unix(to, 0)
			endLayout := "15:04"
			if since.Local().Format("Jan 02") != until.Local().Format("Jan 02") {
				endLayout = "Jan 02 15:04"
			}
			window = since.Local().Format("Jan 02 15:04") + " – " + until.Local().Format(endLayout)
		}
	}

	sortSlowest := q.Get("sort") == "slowest"

	filter := store.ListFilter{
		Type:        sec.Type,
		Since:       since,
		Until:       until,
		Status:      q.Get("status"),
		UserID:      q.Get("user"),
		Group:       q.Get("group"),
		Search:      q.Get("search"),
		SortSlowest: sortSlowest,
		Limit:       perPage,
		Offset:      (page - 1) * perPage,
	}

	records, err := s.store.List(ctx, filter)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	count, err := s.store.Count(ctx, filter)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	var groups, slowGroups []store.GroupStat
	if sec.GroupLabelExpr != "" && page == 1 && q.Get("search") == "" && q.Get("group") == "" {
		groups, err = s.store.GroupStats(ctx, sec.Type, sec.GroupLabelExpr, "count", since, 10)
		if err != nil {
			httpError(w, s.log, err)
			return
		}
		if sec.SlowTitle != "" {
			slowGroups, err = s.store.GroupStats(ctx, sec.Type, sec.GroupLabelExpr, "avg", since, 10)
			if err != nil {
				httpError(w, s.log, err)
				return
			}
		}
	}

	// With a drag/click window active the chart zooms into it, re-bucketed
	// at finer resolution; otherwise it shows the full selected range.
	chartSince, chartUntil := since, until
	span := time.Since(chartSince)
	if !until.IsZero() {
		span = until.Sub(chartSince)
	}
	bm := bucketMinutes(span)
	timeline, err := s.store.TimelineByClass(ctx, sec.Type, chartSince, chartUntil, bm)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	qs := q
	qs.Del("page")
	base.QueryString = qs.Encode()

	clearQS := q
	clearQS.Del("page")
	clearQS.Del("from")
	clearQS.Del("to")
	clearQS.Del("sort")

	s.render(w, "section.html", map[string]any{
		"Base":       base,
		"Section":    sec,
		"Records":    records,
		"Count":      count,
		"Page":       page,
		"PerPage":    perPage,
		"HasPrev":    page > 1,
		"HasNext":    int64(page*perPage) < count,
		"Groups":     groups,
		"SlowGroups": slowGroups,
		"Search":     q.Get("search"),
		"Status":     q.Get("status"),
		"GroupParam": q.Get("group"),
		"Slowest":    sortSlowest,
		"Window":     window,
		"FromParam":  q.Get("from"),
		"ToParam":    q.Get("to"),
		"ClearQS":    clearQS.Encode(),
		"Chart": chartJSON(timeline, bm, chartOpts{
			DrillURL:    "/section/" + sec.Slug,
			DrillParams: clearQS.Encode(),
			OKLabel:     sec.OKLabel,
			WarnLabel:   sec.WarnLabel,
			ErrLabel:    sec.ErrLabel,
		}),
	})
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rec, err := s.store.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base, _ := s.base(r, "")
	s.render(w, "record.html", map[string]any{"Base": base, "Record": rec})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	trace := r.PathValue("trace")
	records, err := s.store.List(r.Context(), store.ListFilter{TraceID: trace, Limit: 500})
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	// Show execution order: oldest first.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	base, _ := s.base(r, "")
	s.render(w, "trace.html", map[string]any{"Base": base, "Trace": trace, "Records": records})
}

func httpError(w http.ResponseWriter, log *slog.Logger, err error) {
	log.Error("request failed", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// bucketMinutes picks a bucket size that keeps the chart readable for any
// span, down to one-minute buckets for tight drag-zoom windows.
func bucketMinutes(span time.Duration) int {
	switch {
	case span <= 45*time.Minute:
		return 1
	case span <= 2*time.Hour:
		return 2
	case span <= 6*time.Hour:
		return 5
	case span <= 25*time.Hour:
		return 30
	case span <= 8*24*time.Hour:
		return 180
	default:
		return 720
	}
}

// chartOpts configures the client-side chart renderer.
type chartOpts struct {
	DrillURL    string `json:"drillUrl,omitempty"`
	DrillParams string `json:"drillParams,omitempty"`
	OKLabel     string `json:"okLabel,omitempty"`
	WarnLabel   string `json:"warnLabel,omitempty"`
	ErrLabel    string `json:"errLabel,omitempty"`
}

// chartJSON serializes classified buckets plus renderer options for the
// interactive chart: per-bucket status-class counts, avg duration, and the
// bucket's unix window so clicks can drill into the exact time range.
func chartJSON(buckets []store.ClassBucket, bucketMinutes int, opts chartOpts) template.JS {
	type pt struct {
		T     string  `json:"t"`
		From  int64   `json:"from"`
		To    int64   `json:"to"`
		OK    int64   `json:"ok"`
		Warn  int64   `json:"warn"`
		Err   int64   `json:"err"`
		Other int64   `json:"other"`
		D     float64 `json:"d"`
	}
	span := time.Duration(bucketMinutes) * time.Minute
	layout := "15:04"
	if bucketMinutes >= 180 {
		layout = "Jan 02 15:04"
	}
	pts := make([]pt, len(buckets))
	for i, b := range buckets {
		pts[i] = pt{
			T:     b.Bucket.Local().Format(layout),
			From:  b.Bucket.Unix(),
			To:    b.Bucket.Add(span).Unix(),
			OK:    b.OK,
			Warn:  b.Warn,
			Err:   b.Err,
			Other: b.Other,
			D:     b.AvgMs,
		}
	}
	b, err := json.Marshal(map[string]any{"data": pts, "opts": opts})
	if err != nil {
		return `{"data":[],"opts":{}}`
	}
	return template.JS(b)
}

// Run serves HTTP until ctx is done.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("web panel listening", "addr", addr)
	err := srv.ListenAndServe()
	if err != nil && !strings.Contains(err.Error(), "Server closed") {
		return err
	}
	return nil
}
