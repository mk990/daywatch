package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	// GroupLang enables client-side syntax highlighting of group labels.
	GroupLang string
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
	store        *store.Store
	log          *slog.Logger
	tmpl         *template.Template
	sections     []Section
	bySlug       map[string]*Section
	hub          *Hub
	auth         AuthConfig
	loginLimiter *loginLimiter
	alertTester  AlertTester
}

// Hub returns the live-update hub; the ingest pipeline calls Notify on it.
func (s *Server) Hub() *Hub { return s.hub }

func New(st *store.Store, log *slog.Logger, auth AuthConfig) (*Server, error) {
	s := &Server{store: st, log: log, bySlug: map[string]*Section{}, hub: NewHub(), auth: auth, loginLimiter: newLoginLimiter()}
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
		// countfmt renders a possibly-capped Count: values past the cap show
		// as "50000+" since Count stops counting there.
		"countfmt": func(n int64) string {
			if n > store.CountCap {
				return strconv.FormatInt(store.CountCap, 10) + "+"
			}
			return strconv.FormatInt(n, 10)
		},
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
	mux.HandleFunc("GET /exceptions", s.handleExceptions)
	mux.HandleFunc("GET /exceptions/{group}", s.handleExceptionDetail)
	mux.HandleFunc("POST /exceptions/{group}/status", s.handleExceptionStatus)
	mux.HandleFunc("GET /users", s.handleUsers)
	mux.HandleFunc("GET /user/{uid}", s.handleUserDetail)
	mux.HandleFunc("GET /record/{id}", s.handleRecord)
	mux.HandleFunc("GET /trace/{trace}", s.handleTrace)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("GET /apps", s.handleApps)
	mux.HandleFunc("POST /apps", s.handleAppCreate)
	mux.HandleFunc("POST /apps/{id}/delete", s.handleAppDelete)
	mux.HandleFunc("POST /apps/{id}/regenerate", s.handleAppRegenerate)
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

// appLink is one entry in a topbar switcher (apps or stages).
type appLink struct {
	Label  string
	URL    string
	Active bool
}

// baseData is embedded in every page render.
type baseData struct {
	Sections   []Section
	ActiveSlug string
	Range      string
	RangeLabel string
	// QueryString and ScopeQS are pre-encoded query fragments; template.URL
	// stops html/template from re-escaping the & and = when they are
	// spliced into href attributes.
	QueryString template.URL
	AuthEnabled bool
	App         string       // selected app filter ("" = all)
	Stage       string       // selected execution-stage filter ("" = all)
	ScopeQS     template.URL // "&app=x&stage=y" fragment for building links
	AppSwitch   []appLink    // topbar switcher; empty hides it
	StageSwitch []appLink    // topbar stage switcher; empty hides it
}

func (s *Server) base(r *http.Request, active string) (baseData, time.Time) {
	q := r.URL.Query()
	rng := q.Get("range")
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

	// Registered apps come from the database so panel-created apps show
	// up immediately; the app filter only accepts registered names.
	apps, err := s.store.AppNames(r.Context())
	if err != nil {
		s.log.Error("listing apps failed", "error", err)
	}
	app := ""
	if want := q.Get("app"); want != "" && slices.Contains(apps, want) {
		app = want
	}

	// Stages are whatever the packages have reported (production, staging,
	// …); the filter only accepts seen values so links stay canonical.
	stages, err := s.store.StageNames(r.Context())
	if err != nil {
		s.log.Error("listing stages failed", "error", err)
	}
	stage := ""
	if want := q.Get("stage"); want != "" && slices.Contains(stages, want) {
		stage = want
	}

	b := baseData{
		Sections:    s.sections,
		ActiveSlug:  active,
		Range:       rng,
		RangeLabel:  label,
		AuthEnabled: s.auth.Enabled(),
		App:         app,
		Stage:       stage,
	}
	scope := url.Values{}
	if app != "" {
		scope.Set("app", app)
	}
	if stage != "" {
		scope.Set("stage", stage)
	}
	if len(scope) > 0 {
		b.ScopeQS = template.URL("&" + scope.Encode())
		// Range-picker links append QueryString; keep the scope by default.
		// Handlers that set their own QueryString include these params anyway.
		b.QueryString = template.URL(scope.Encode())
	}
	if len(apps) > 1 {
		b.AppSwitch = scopeSwitch(r, "app", "All apps", apps, app)
	}
	if len(stages) > 1 {
		b.StageSwitch = scopeSwitch(r, "stage", "All stages", stages, stage)
	}
	return b, since
}

// scopeSwitch builds "All | a | b …" links that preserve the current page
// and query, swapping only the given scope parameter (app or stage).
func scopeSwitch(r *http.Request, param, allLabel string, names []string, current string) []appLink {
	linkTo := func(val string) string {
		q := r.URL.Query()
		q.Del(param)
		// A different scope means different data: drop drill windows and paging.
		q.Del("from")
		q.Del("to")
		q.Del("page")
		if val != "" {
			q.Set(param, val)
		}
		u := r.URL.Path
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
		return u
	}
	links := []appLink{{Label: allLabel, URL: linkTo(""), Active: current == ""}}
	for _, name := range names {
		links = append(links, appLink{Label: name, URL: linkTo(name), Active: current == name})
	}
	return links
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
	app, stage := base.App, base.Stage

	counts, err := s.store.CountsByType(ctx, app, stage, since)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	routes, err := s.store.RequestStats(ctx, app, stage, since, 15)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	slowQueries, err := s.store.GroupStats(ctx, app, stage, "query", "data->>'sql'", "total", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	exceptions, err := s.store.GroupStats(ctx, app, stage, "exception", "concat(data->>'class', ': ', data->>'message')", "count", since, 10)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	bm := bucketMinutes(time.Since(since))
	timeline, err := s.store.TimelineByClass(ctx, app, stage, "request", since, time.Time{}, bm)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	recent, err := s.store.List(ctx, store.ListFilter{App: app, Stage: stage, Since: since, Limit: 15})
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
			DrillParams: "range=" + base.Range + string(base.ScopeQS),
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
	// Exceptions and users get dedicated aggregate pages.
	if sec.Slug == "exceptions" || sec.Slug == "users" {
		dest := "/" + sec.Slug
		q := r.URL.Query()
		if sec.Slug == "exceptions" {
			if g := q.Get("group"); g != "" {
				dest += "/" + g
				q.Del("group")
			}
		}
		if enc := q.Encode(); enc != "" {
			dest += "?" + enc
		}
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
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
		App:         base.App,
		Stage:       base.Stage,
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
		groups, err = s.store.GroupStats(ctx, base.App, base.Stage, sec.Type, sec.GroupLabelExpr, "count", since, 10)
		if err != nil {
			httpError(w, s.log, err)
			return
		}
		if sec.SlowTitle != "" {
			slowGroups, err = s.store.GroupStats(ctx, base.App, base.Stage, sec.Type, sec.GroupLabelExpr, "avg", since, 10)
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
	timeline, err := s.store.TimelineByClass(ctx, base.App, base.Stage, sec.Type, chartSince, chartUntil, bm)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	qs := q
	qs.Del("page")
	base.QueryString = template.URL(qs.Encode())

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
		"ClearQS":    template.URL(clearQS.Encode()),
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

// wfRow is one row of the trace waterfall: a record positioned on the
// trace's timeline by start offset and duration.
type wfRow struct {
	Rec       store.Record
	Label     string
	OffsetPct float64
	WidthPct  float64
	Class     string // bar color class, derived from the record type
	Err       bool
	Marker    bool    // durationless records render as a point, not a bar
	EndPct    float64 // OffsetPct + WidthPct, where the duration label sits
}

var wfClasses = map[string]string{
	"request":          "wf-request",
	"query":            "wf-query",
	"cache-event":      "wf-cache",
	"outgoing-request": "wf-outgoing",
	"job-attempt":      "wf-job",
	"queued-job":       "wf-job",
	"scheduled-task":   "wf-job",
	"mail":             "wf-mail",
	"notification":     "wf-mail",
	"exception":        "wf-exception",
	"log":              "wf-log",
}

// recordSummary picks the most descriptive payload field for a record.
func recordSummary(rec store.Record) string {
	for _, key := range []string{"url", "sql", "message", "name", "class", "key", "id"} {
		if v := anyToString(rec.Data[key]); v != "" {
			if key == "url" {
				if m := anyToString(rec.Data["method"]); m != "" {
					v = m + " " + v
				}
			}
			return v
		}
	}
	return rec.Type
}

func statusIsErr(status string) bool {
	switch {
	case len(status) == 3 && status[0] == '5':
		return true
	case status == "failed" || status == "unhandled" || status == "error" ||
		status == "critical" || status == "emergency" || status == "alert":
		return true
	}
	return false
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

	var rows []wfRow
	var spanUS int64
	if len(records) > 0 {
		start := records[0].TS
		end := start
		for _, rec := range records {
			if rec.TS.Before(start) {
				start = rec.TS
			}
			if e := rec.TS.Add(time.Duration(rec.Duration) * time.Microsecond); e.After(end) {
				end = e
			}
		}
		spanUS = end.Sub(start).Microseconds()
		if spanUS <= 0 {
			spanUS = 1000 // degenerate trace: avoid dividing by zero
		}
		for _, rec := range records {
			row := wfRow{
				Rec:       rec,
				Label:     trunc(recordSummary(rec), 70),
				OffsetPct: float64(rec.TS.Sub(start).Microseconds()) / float64(spanUS) * 100,
				WidthPct:  float64(rec.Duration) / float64(spanUS) * 100,
				Class:     wfClasses[rec.Type],
				Err:       statusIsErr(rec.Status),
				Marker:    rec.Duration == 0,
			}
			if row.Class == "" {
				row.Class = "wf-log"
			}
			if !row.Marker && row.WidthPct < 0.5 {
				row.WidthPct = 0.5 // keep short spans visible
			}
			if row.OffsetPct > 99.5 {
				row.OffsetPct = 99.5
			}
			row.EndPct = row.OffsetPct + row.WidthPct
			if row.EndPct > 85 {
				row.EndPct = 85 // keep the duration label inside the track
			}
			rows = append(rows, row)
		}
	}

	// Time-axis tick labels at 0/25/50/75/100% of the span.
	type tick struct {
		Label string
		Left  int
	}
	var ticks []tick
	for i := 0; i <= 4; i++ {
		us := float64(spanUS) * float64(i) / 4
		var label string
		switch {
		case us >= 1e6:
			label = fmt.Sprintf("%.2fs", us/1e6)
		case us >= 1e3:
			label = fmt.Sprintf("%.1fms", us/1e3)
		default:
			label = fmt.Sprintf("%.0fµs", us)
		}
		ticks = append(ticks, tick{Label: label, Left: i * 25})
	}

	base, _ := s.base(r, "")
	s.render(w, "trace.html", map[string]any{
		"Base":    base,
		"Trace":   trace,
		"Records": records,
		"Rows":    rows,
		"SpanUS":  spanUS,
		"Ticks":   ticks,
	})
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
		P95   float64 `json:"p95"`
		P99   float64 `json:"p99"`
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
			P95:   b.P95,
			P99:   b.P99,
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
