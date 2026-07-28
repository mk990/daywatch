package store

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This suite is opt-in because it migrates and writes to the configured
// database. CI or local development should point it at a disposable database.
func TestPostgresScopesAndRollups(t *testing.T) {
	dsn := os.Getenv("DAYWATCH_INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("DAYWATCH_INTEGRATION_DATABASE_URL is not set")
	}
	ctx := t.Context()

	// Start from the pre-scope exception schema to exercise its migration.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		DROP TABLE IF EXISTS exception_status;
		CREATE TABLE exception_status (
			group_hash TEXT PRIMARY KEY,
			status TEXT NOT NULL CHECK (status IN ('resolved','ignored')),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	pool.Close()
	if err != nil {
		t.Fatal(err)
	}

	st, err := New(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.pool.Exec(ctx, `
		TRUNCATE records, rollups_hourly, group_rollups_hourly,
		         exception_status, alert_events, alert_rules, apps
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	add := func(app string, at time.Time, typ, group, userID, name string) {
		t.Helper()
		payload := map[string]any{
			"t": typ, "timestamp": float64(at.UnixNano()) / 1e9,
			"_group": group, "user": userID, "duration": 100,
		}
		if typ == "user" {
			payload["id"], payload["name"] = userID, name
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.InsertRecords(ctx, []json.RawMessage{raw}, app); err != nil {
			t.Fatal(err)
		}
	}

	add("scope-a", base.Add(40*time.Minute), "user", "", "42", "Alice")
	add("scope-b", base.Add(41*time.Minute), "user", "", "42", "Bob")
	names, err := st.UserNames(ctx, "scope-a", "", []string{"42"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names["42"]; got != "Alice" {
		t.Fatalf("scope-a user 42 = %q, want Alice", got)
	}
	users, err := st.UserStats(ctx, "", "", base, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].App == users[1].App {
		t.Fatalf("all-app user stats = %+v, want separate app rows", users)
	}

	add("scope-a", base.Add(42*time.Minute), "exception", "shared-group", "", "")
	add("scope-b", base.Add(43*time.Minute), "exception", "shared-group", "", "")
	if err := st.SetExceptionStatus(ctx, "scope-a", "", "shared-group", "resolved"); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetExceptionGroup(ctx, "scope-a", "", "shared-group")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.GetExceptionGroup(ctx, "scope-b", "", "shared-group")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "resolved" || b.Status != "open" {
		t.Fatalf("scoped exception states = %q/%q, want resolved/open", a.Status, b.Status)
	}
	add("scope-a", base.Add(44*time.Minute), "exception", "shared-group", "", "")
	a, err = st.GetExceptionGroup(ctx, "scope-a", "", "shared-group")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "open" {
		t.Fatalf("recurring scope-a exception state = %q, want open", a.Status)
	}

	// The first request is before the exact range boundary but within the
	// same hour; it must not leak into CountsByType through the hourly rollup.
	add("scope-a", base.Add(10*time.Minute), "request", "route-a", "", "")
	add("scope-a", base.Add(40*time.Minute), "request", "route-a", "", "")
	add("scope-a", base.Add(70*time.Minute), "request", "route-a", "", "")
	if err := st.UpdateRollups(ctx, base); err != nil {
		t.Fatal(err)
	}
	counts, err := st.CountsByType(ctx, "scope-a", "", base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, count := range counts {
		if count.Type == "request" {
			requests = count.Count
		}
	}
	if requests != 2 {
		t.Fatalf("request count = %d, want 2", requests)
	}
	groups, err := st.GroupStats(ctx, "scope-a", "", "request",
		"concat(data->>'method', ' ', data->>'url')", "count", base.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Count != 2 {
		t.Fatalf("group stats = %+v, want one group with count 2", groups)
	}
	routes, err := st.requestStatsFromRollups(ctx, "scope-a", "", base.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Count != 2 {
		t.Fatalf("route stats = %+v, want one route with count 2", routes)
	}
}
