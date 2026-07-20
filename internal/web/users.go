package web

import (
	"net/http"

	"github.com/mk/daywatch/internal/store"
)

// handleUsers lists the most active users in the selected window.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	base, since := s.base(r, "users")
	users, err := s.store.UserStats(r.Context(), base.App, base.Stage, since, 50)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	s.render(w, "users.html", map[string]any{
		"Base":  base,
		"Users": users,
	})
}

// handleUserDetail shows one user's identity plus their recent records.
func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	base, _ := s.base(r, "users")

	stat, err := s.store.GetUserStat(r.Context(), base.App, base.Stage, uid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	records, err := s.store.List(r.Context(), store.ListFilter{App: base.App, Stage: base.Stage, UserID: uid, Limit: 50})
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	// Waterfall-style summaries for the mixed-type activity list.
	type row struct {
		Rec     store.Record
		Summary string
		Err     bool
	}
	rows := make([]row, 0, len(records))
	for _, rec := range records {
		rows = append(rows, row{Rec: rec, Summary: trunc(recordSummary(rec), 90), Err: statusIsErr(rec.Status)})
	}

	s.render(w, "user.html", map[string]any{
		"Base": base,
		"U":    stat,
		"Rows": rows,
	})
}
