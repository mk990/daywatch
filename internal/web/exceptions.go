package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mk/daywatch/internal/store"
)

// Frame is one parsed stack-trace frame from an exception record.
type Frame struct {
	File   string
	Line   int
	Source string     // Class->method() that executed this frame
	App    bool       // true for application code (not vendor/)
	Code   []CodeLine // surrounding source lines, when captured
}

type CodeLine struct {
	No      int
	Text    string
	Current bool // the line the frame points at
}

// parseTrace decodes the Nightwatch exception trace payload: a JSON string
// holding [{file: "path:line", source: "...", code: {"12": "..."} | null}].
func parseTrace(data map[string]any) []Frame {
	raw, _ := data["trace"].(string)
	if raw == "" {
		return nil
	}
	var frames []struct {
		File   string            `json:"file"`
		Source string            `json:"source"`
		Code   map[string]string `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &frames); err != nil {
		return nil
	}
	out := make([]Frame, 0, len(frames))
	for _, f := range frames {
		fr := Frame{File: f.File, Source: f.Source}
		if i := strings.LastIndex(f.File, ":"); i > 0 {
			if n, err := strconv.Atoi(f.File[i+1:]); err == nil {
				fr.File, fr.Line = f.File[:i], n
			}
		}
		fr.App = fr.File != "" && !strings.HasPrefix(fr.File, "vendor/")
		for no, text := range f.Code {
			n, err := strconv.Atoi(no)
			if err != nil {
				continue
			}
			fr.Code = append(fr.Code, CodeLine{No: n, Text: text, Current: n == fr.Line})
		}
		sort.Slice(fr.Code, func(i, j int) bool { return fr.Code[i].No < fr.Code[j].No })
		out = append(out, fr)
	}
	return out
}

var exceptionTabs = []string{"open", "resolved", "ignored", "all"}

// handleExceptions is the group-centric exceptions index: timeline chart,
// triage tabs, and one row per distinct exception.
func (s *Server) handleExceptions(w http.ResponseWriter, r *http.Request) {
	base, since := s.base(r, "exceptions")
	ctx := r.Context()
	q := r.URL.Query()

	tab := q.Get("tab")
	valid := false
	for _, t := range exceptionTabs {
		if t == tab {
			valid = true
			break
		}
	}
	if !valid {
		tab = "open"
	}
	statusFilter := tab
	if tab == "all" {
		statusFilter = ""
	}

	// Chart drill/drag windows narrow the group listing too.
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

	search := q.Get("search")
	groups, err := s.store.ExceptionGroups(ctx, since, until, statusFilter, search, 50)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	tabCounts, err := s.store.ExceptionStatusCounts(ctx, since, until)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	span := time.Since(since)
	if !until.IsZero() {
		span = until.Sub(since)
	}
	bm := bucketMinutes(span)
	timeline, err := s.store.TimelineByClass(ctx, "exception", since, until, bm)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	qs := q
	base.QueryString = qs.Encode()
	clearQS := q
	clearQS.Del("from")
	clearQS.Del("to")

	s.render(w, "exceptions.html", map[string]any{
		"Base":      base,
		"Groups":    groups,
		"Tab":       tab,
		"Tabs":      exceptionTabs,
		"TabCounts": tabCounts,
		"AllCount":  tabCounts["open"] + tabCounts["resolved"] + tabCounts["ignored"],
		"Search":    search,
		"Window":    window,
		"FromParam": q.Get("from"),
		"ToParam":   q.Get("to"),
		"ClearQS":   clearQS.Encode(),
		"Chart": chartJSON(timeline, bm, chartOpts{
			DrillURL:    "/exceptions",
			DrillParams: clearQS.Encode(),
			OKLabel:     "Handled",
			ErrLabel:    "Unhandled",
		}),
	})
}

// handleExceptionDetail shows one group: header stats, triage actions, the
// formatted stack trace of an occurrence, and recent occurrences.
func (s *Server) handleExceptionDetail(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	ctx := r.Context()

	g, err := s.store.GetExceptionGroup(ctx, group)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Default to the latest occurrence; ?record=N inspects an older one.
	recID := g.LastID
	if n, err := strconv.ParseInt(r.URL.Query().Get("record"), 10, 64); err == nil && n > 0 {
		recID = n
	}
	rec, err := s.store.Get(ctx, recID)
	if err != nil || rec.Group != group {
		rec, err = s.store.Get(ctx, g.LastID)
		if err != nil {
			httpError(w, s.log, err)
			return
		}
	}

	occurrences, err := s.store.List(ctx, store.ListFilter{Type: "exception", Group: group, Limit: 50})
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	frames := parseTrace(rec.Data)
	appFrames := 0
	for _, f := range frames {
		if f.App {
			appFrames++
		}
	}

	base, _ := s.base(r, "exceptions")
	s.render(w, "exception.html", map[string]any{
		"Base":        base,
		"G":           g,
		"Record":      rec,
		"Frames":      frames,
		"AppFrames":   appFrames,
		"Occurrences": occurrences,
	})
}

// handleExceptionStatus applies a triage action (resolve/ignore/reopen).
func (s *Server) handleExceptionStatus(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	status := map[string]string{
		"resolve": "resolved",
		"ignore":  "ignored",
		"reopen":  "open",
	}[r.FormValue("action")]
	if status == "" {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	if err := s.store.SetExceptionStatus(r.Context(), group, status); err != nil {
		httpError(w, s.log, err)
		return
	}
	s.hub.Notify()
	back := r.FormValue("back")
	if back == "" || !strings.HasPrefix(back, "/") {
		back = "/exceptions/" + group
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
