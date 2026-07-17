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
}

type Column struct {
	Header string
	Value  func(r store.Record) template.HTML
}

type Server struct {
	store    *store.Store
	log      *slog.Logger
	tmpl     *template.Template
	sections []Section
	bySlug   map[string]*Section
}

func New(st *store.Store, log *slog.Logger) (*Server, error) {
	s := &Server{store: st, log: log, bySlug: map[string]*Section{}}
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
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	return mux
}

// baseData is embedded in every page render.
type baseData struct {
	Sections    []Section
	ActiveSlug  string
	Range       string
	RangeLabel  string
	QueryString string
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
		Sections:   s.sections,
		ActiveSlug: active,
		Range:      rng,
		RangeLabel: label,
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
	slowQueries, err := s.store.GroupStats(ctx, "query", "data->>'sql'", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	exceptions, err := s.store.GroupStats(ctx, "exception", "concat(data->>'class', ': ', data->>'message')", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	timeline, err := s.store.Timeline(ctx, "request", since, bucketMinutes(since))
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
		"Timeline":    timelineJSON(timeline),
		"Recent":      recent,
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

	filter := store.ListFilter{
		Type:   sec.Type,
		Since:  since,
		Status: q.Get("status"),
		UserID: q.Get("user"),
		Group:  q.Get("group"),
		Search: q.Get("search"),
		Limit:  perPage,
		Offset: (page - 1) * perPage,
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

	var groups []store.GroupStat
	if sec.GroupLabelExpr != "" && page == 1 && q.Get("search") == "" && q.Get("group") == "" {
		groups, err = s.store.GroupStats(ctx, sec.Type, sec.GroupLabelExpr, since, 10)
		if err != nil {
			httpError(w, s.log, err)
			return
		}
	}

	qs := q
	qs.Del("page")
	base.QueryString = qs.Encode()

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
		"Search":     q.Get("search"),
		"Status":     q.Get("status"),
		"GroupParam": q.Get("group"),
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

func bucketMinutes(since time.Time) int {
	span := time.Since(since)
	switch {
	case span <= 2*time.Hour:
		return 2
	case span <= 25*time.Hour:
		return 30
	case span <= 8*24*time.Hour:
		return 180
	default:
		return 720
	}
}

func timelineJSON(buckets []store.TimeBucket) template.JS {
	type pt struct {
		T string  `json:"t"`
		C int64   `json:"c"`
		D float64 `json:"d"`
	}
	pts := make([]pt, len(buckets))
	for i, b := range buckets {
		pts[i] = pt{T: b.Bucket.Local().Format("15:04 Jan 02"), C: b.Count, D: b.AvgMs}
	}
	b, err := json.Marshal(pts)
	if err != nil {
		return "[]"
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
