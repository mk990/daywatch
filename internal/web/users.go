package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/mk990/daywatch/internal/store"
)

// withUserNames fills Record.UserName for every record carrying a user id,
// in one lookup, so lists can show "Name #42" instead of a bare number.
// A failure only costs the names, so it logs and leaves them blank.
func (s *Server) withUserNames(ctx context.Context, app, stage string, records []store.Record) {
	if app == "" {
		byApp := map[string][]int{}
		for i := range records {
			if records[i].UserID != "" {
				byApp[records[i].App] = append(byApp[records[i].App], i)
			}
		}
		for recordApp, indexes := range byApp {
			seen := map[string]bool{}
			var ids []string
			for _, i := range indexes {
				id := records[i].UserID
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
			names, err := s.store.UserNames(ctx, recordApp, stage, ids)
			if err != nil {
				s.log.Warn("resolving user names failed", "app", recordApp, "error", err)
				continue
			}
			for _, i := range indexes {
				records[i].UserName = names[records[i].UserID]
			}
		}
		return
	}

	seen := map[string]bool{}
	var ids []string
	for _, r := range records {
		if r.UserID != "" && !seen[r.UserID] {
			seen[r.UserID] = true
			ids = append(ids, r.UserID)
		}
	}
	if len(ids) == 0 {
		return
	}
	names, err := s.store.UserNames(ctx, app, stage, ids)
	if err != nil {
		s.log.Warn("resolving user names failed", "error", err)
		return
	}
	for i := range records {
		records[i].UserName = names[records[i].UserID]
	}
}

// userCell renders a user id as a link, with the resolved name in front of
// it when one is known. The id always stays visible: it is what appears in
// the raw payload, so hiding it behind a name would make records harder to
// correlate.
func userCell(r store.Record) template.HTML {
	if r.UserID == "" {
		return ""
	}
	id := template.HTMLEscapeString(r.UserID)
	href := "/user/" + url.PathEscape(r.UserID)
	q := url.Values{}
	if r.App != "" {
		q.Set("app", r.App)
	}
	if r.Stage != "" {
		q.Set("stage", r.Stage)
	}
	if enc := q.Encode(); enc != "" {
		href += "?" + enc
	}
	href = template.HTMLEscapeString(href)
	if r.UserName == "" {
		return template.HTML(`<a class="user-ref" href="` + href + `"><span class="user-id">#` + id + `</span></a>`)
	}
	return template.HTML(fmt.Sprintf(
		`<a class="user-ref" href="%s"><span class="user-name">%s</span><span class="user-id">#%s</span></a>`,
		href, template.HTMLEscapeString(trunc(r.UserName, 28)), id))
}

// handleUsers lists the most active users in the selected window.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	base, since := s.base(r, "users")
	users, err := s.store.UserStats(r.Context(), base.App, base.Stage, since, 50)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	s.render(w, r, "users.html", map[string]any{
		"Base":  base,
		"Users": users,
	})
}

// userInitials builds an avatar label from the best available identity,
// by rune so non-Latin names work.
func userInitials(u *store.UserStat) string {
	src := u.Name
	if src == "" {
		src = u.Username
	}
	if src == "" {
		return "#"
	}
	var out []rune
	for word := range strings.FieldsSeq(src) {
		r := []rune(word)
		if len(r) == 0 {
			continue
		}
		out = append(out, unicode.ToUpper(r[0]))
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "#"
	}
	return string(out)
}

// typeChip is one entry in the user page's record-type filter.
type typeChip struct {
	Type   string
	Label  string
	Count  int64
	URL    string
	Active bool
}

// activityRow is one line of a user's mixed-type activity feed, pre-rendered
// because the markup differs per record type.
type activityRow struct {
	Rec     store.Record
	Summary template.HTML
	Status  template.HTML
	Class   string // per-type colour class, shared with the trace waterfall
}

// handleUserDetail shows one user's identity, a breakdown of what they did,
// and their recent records, optionally narrowed to one record type.
func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	base, _ := s.base(r, "users")
	ctx := r.Context()

	stat, err := s.store.GetUserStat(ctx, base.App, base.Stage, uid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	counts, err := s.store.UserTypeCounts(ctx, base.App, base.Stage, uid)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	// An unknown type falls back to everything rather than an empty page.
	typ := r.URL.Query().Get("type")
	if typ != "" && !slices.ContainsFunc(counts, func(c store.TypeCount) bool { return c.Type == typ }) {
		typ = ""
	}

	records, err := s.store.List(ctx, store.ListFilter{
		App: base.App, Stage: base.Stage, UserID: uid, Type: typ, Limit: 50,
	})
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	rows := make([]activityRow, 0, len(records))
	for _, rec := range records {
		rows = append(rows, activityRow{
			Rec:     rec,
			Summary: activitySummary(rec),
			Status:  statusBadge(rec),
			Class:   wfClass(rec.Type),
		})
	}

	link := func(t string) string {
		u := "/user/" + url.PathEscape(uid) + "?range=" + url.QueryEscape(base.Range)
		if t != "" {
			u += "&type=" + url.QueryEscape(t)
		}
		return u + string(base.ScopeQS)
	}
	chips := []typeChip{{Label: "all", Count: stat.Events, URL: link(""), Active: typ == ""}}
	for _, c := range counts {
		chips = append(chips, typeChip{
			Type: c.Type, Label: c.Type, Count: c.Count, URL: link(c.Type), Active: typ == c.Type,
		})
	}

	s.render(w, r, "user.html", map[string]any{
		"Base":  base,
		"U":     stat,
		"Rows":  rows,
		"Chips": chips,
		"Type":  typ,
	})
}

// activitySummary renders a record's summary with markup suited to its type:
// SQL goes through the syntax highlighter, requests get a coloured verb chip,
// exceptions lead with the class name.
func activitySummary(r store.Record) template.HTML {
	switch r.Type {
	case "query":
		if sql := anyToString(r.Data["sql"]); sql != "" {
			return hlSpan("sql", trunc(sql, 140))
		}
	case "request", "outgoing-request":
		if path := anyToString(r.Data["url"]); path != "" {
			out := ""
			if m := strings.ToUpper(anyToString(r.Data["method"])); m != "" {
				esc := template.HTMLEscapeString(m)
				out = `<span class="verb" data-verb="` + esc + `">` + esc + `</span> `
			}
			return template.HTML(out + `<span class="path">` +
				template.HTMLEscapeString(trunc(path, 110)) + `</span>`)
		}
	case "exception":
		class, msg := anyToString(r.Data["class"]), anyToString(r.Data["message"])
		if class != "" || msg != "" {
			// dir="auto" isolates the message: a Persian or Arabic message
			// renders right-to-left without dragging the class name with it.
			return template.HTML(`<span class="exc-class">` +
				template.HTMLEscapeString(trunc(class, 40)) + `</span> <span class="exc-text" dir="auto">` +
				template.HTMLEscapeString(trunc(msg, 100)) + `</span>`)
		}
	}
	return esc(trunc(recordSummary(r), 120))
}
