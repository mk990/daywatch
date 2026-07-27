package web

import (
	"strings"
	"testing"

	"github.com/mk990/daywatch/internal/store"
)

func TestUserCell(t *testing.T) {
	// No id: nothing to render.
	if got := userCell(store.Record{}); got != "" {
		t.Errorf("userCell with no user = %q, want empty", got)
	}

	// Unknown name: the id still links, so the row is not a dead end.
	got := string(userCell(store.Record{UserID: "42"}))
	for _, want := range []string{`href="/user/42"`, "#42"} {
		if !strings.Contains(got, want) {
			t.Errorf("userCell = %q, missing %q", got, want)
		}
	}

	// Known name: both the name and the raw id are shown.
	got = string(userCell(store.Record{UserID: "42", UserName: "مریم کریمی"}))
	for _, want := range []string{`href="/user/42"`, "مریم کریمی", "#42"} {
		if !strings.Contains(got, want) {
			t.Errorf("userCell = %q, missing %q", got, want)
		}
	}
}

func TestUserCellEscapes(t *testing.T) {
	got := string(userCell(store.Record{UserID: `1"><script>`, UserName: `<img onerror=x>`}))
	if strings.Contains(got, "<script>") || strings.Contains(got, "<img") {
		t.Fatalf("userCell emitted unescaped markup: %q", got)
	}
}

func TestUserInitials(t *testing.T) {
	cases := []struct {
		name string
		u    store.UserStat
		want string
	}{
		{"two words", store.UserStat{Name: "Ada Lovelace"}, "AL"},
		{"one word", store.UserStat{Name: "ada"}, "A"},
		{"three words take two", store.UserStat{Name: "jean claude van damme"}, "JC"},
		{"falls back to username", store.UserStat{Username: "root"}, "R"},
		{"nothing known", store.UserStat{UserID: "42"}, "#"},
		{"non-latin", store.UserStat{Name: "مریم کریمی"}, "مک"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := userInitials(&c.u); got != c.want {
				t.Fatalf("userInitials = %q, want %q", got, c.want)
			}
		})
	}
}

func TestActivitySummary(t *testing.T) {
	// Queries go through the SQL highlighter.
	got := string(activitySummary(store.Record{Type: "query", Data: map[string]any{"sql": "select * from users"}}))
	if !strings.Contains(got, `data-hl="sql"`) {
		t.Errorf("query summary is not highlighted: %q", got)
	}

	// Requests get a verb chip the stylesheet can colour.
	got = string(activitySummary(store.Record{
		Type: "request", Data: map[string]any{"method": "delete", "url": "/orders/9"},
	}))
	for _, want := range []string{`data-verb="DELETE"`, ">DELETE<", "/orders/9"} {
		if !strings.Contains(got, want) {
			t.Errorf("request summary = %q, missing %q", got, want)
		}
	}

	// Exceptions lead with the class, then the message.
	got = string(activitySummary(store.Record{
		Type: "exception", Data: map[string]any{"class": "RuntimeException", "message": "boom"},
	}))
	if !strings.Contains(got, "exc-class") || !strings.Contains(got, "RuntimeException") || !strings.Contains(got, "boom") {
		t.Errorf("exception summary = %q", got)
	}

	// Anything else falls back to the plain summary, escaped.
	got = string(activitySummary(store.Record{Type: "log", Data: map[string]any{"message": "<b>hi</b>"}}))
	if strings.Contains(got, "<b>") {
		t.Errorf("log summary emitted unescaped markup: %q", got)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{
		"200": "ok", "301": "ok", "404": "warn", "500": "err", "503": "err",
		"handled": "ok", "unhandled": "err", "warning": "warn", "info": "ok",
		"debug": "ok", "notice": "warn", "": "", "weird": "", "2xx": "",
	}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestStatusBadgeMatchesClass(t *testing.T) {
	// A log level the old string-comparison badge missed entirely.
	got := string(statusBadge(store.Record{Status: "info"}))
	if !strings.Contains(got, "badge ok") {
		t.Errorf("statusBadge(info) = %q, want an ok badge", got)
	}
	if got := string(statusBadge(store.Record{})); got != "" {
		t.Errorf("statusBadge with no status = %q, want empty", got)
	}
}
