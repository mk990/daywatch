package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS records (
    id          BIGSERIAL PRIMARY KEY,
    app         TEXT        NOT NULL DEFAULT '',
    type        TEXT        NOT NULL,
    ts          TIMESTAMPTZ NOT NULL,
    trace_id    TEXT        NOT NULL DEFAULT '',
    group_hash  TEXT        NOT NULL DEFAULT '',
    user_id     TEXT        NOT NULL DEFAULT '',
    deploy      TEXT        NOT NULL DEFAULT '',
    server      TEXT        NOT NULL DEFAULT '',
    stage       TEXT        NOT NULL DEFAULT '',
    duration    BIGINT      NOT NULL DEFAULT 0,
    status      TEXT        NOT NULL DEFAULT '',
    data        JSONB       NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE records ADD COLUMN IF NOT EXISTS app TEXT NOT NULL DEFAULT '';
-- summary is the record's most descriptive payload field, extracted on
-- ingest so search hits a small indexed column instead of casting every
-- JSONB payload to text. Adding it with a default is metadata-only.
ALTER TABLE records ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS records_type_ts_idx ON records (type, ts DESC);
CREATE INDEX IF NOT EXISTS records_trace_idx   ON records (trace_id) WHERE trace_id <> '';
CREATE INDEX IF NOT EXISTS records_group_idx   ON records (type, group_hash, ts DESC) WHERE group_hash <> '';
CREATE INDEX IF NOT EXISTS records_ts_idx      ON records (ts);
CREATE INDEX IF NOT EXISTS records_app_idx     ON records (app, type, ts DESC) WHERE app <> '';
CREATE INDEX IF NOT EXISTS records_user_idx    ON records (user_id, ts DESC) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS records_slow_idx    ON records (type, duration DESC) WHERE duration > 0;
-- Tiny partial index: it only covers rows still awaiting a summary, so the
-- startup backfill can find them without scanning the table.
CREATE INDEX IF NOT EXISTS records_nosummary_idx ON records (id) WHERE summary = '';
`

// searchSchema needs pg_trgm, which is a trusted extension on PostgreSQL 13+
// but may be unavailable on managed instances. It is applied separately so a
// failure degrades search to a scan of the (small) summary column instead of
// breaking startup.
const searchSchema = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS records_summary_trgm_idx ON records USING gin (summary gin_trgm_ops);
`

// Record is one Nightwatch event with hot columns extracted from the raw payload.
type Record struct {
	ID       int64
	App      string
	Type     string
	TS       time.Time
	TraceID  string
	Group    string
	UserID   string
	Deploy   string
	Server   string
	Stage    string
	Duration int64
	Status   string
	Summary  string
	Data     map[string]any
	// UserName is not stored: it is filled in from the user's latest "user"
	// record so views can show a name next to a bare numeric UserID.
	UserName string
}

type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func New(ctx context.Context, databaseURL string, log *slog.Logger) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	s := &Store{pool: pool, log: log}

	// The database container may still be starting; retry briefly.
	deadline := time.Now().Add(60 * time.Second)
	for {
		err = s.migrate(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
		log.Warn("database not ready, retrying", "error", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return err
	}
	if err := s.migrateExceptions(ctx); err != nil {
		return err
	}
	if err := s.migrateApps(ctx); err != nil {
		return err
	}
	if err := s.migrateRollups(ctx); err != nil {
		return err
	}
	if err := s.migrateAlerts(ctx); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, searchSchema); err != nil {
		s.log.Warn("pg_trgm unavailable: search will scan the summary column instead of using an index", "error", err)
	}
	return nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable. It backs /healthz, so it
// takes a connection from the pool rather than trusting cached state.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InsertRecords parses raw record objects and batch-inserts them under the
// given app ("" when token validation is off).
func (s *Store) InsertRecords(ctx context.Context, raw []json.RawMessage, app string) (int, error) {
	rows := make([][]any, 0, len(raw))
	var exceptionGroups []string
	for _, r := range raw {
		var m map[string]any
		if err := json.Unmarshal(r, &m); err != nil {
			s.log.Warn("skipping malformed record", "error", err)
			continue
		}
		rec := extract(m)
		if rec.Type == "" {
			s.log.Warn("skipping record without type")
			continue
		}
		if rec.Type == "exception" && rec.Group != "" {
			exceptionGroups = append(exceptionGroups, rec.Group)
		}
		rows = append(rows, []any{
			app, rec.Type, rec.TS, rec.TraceID, rec.Group, rec.UserID,
			rec.Deploy, rec.Server, rec.Stage, rec.Duration, rec.Status, rec.Summary, r,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}

	n, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"records"},
		[]string{"app", "type", "ts", "trace_id", "group_hash", "user_id", "deploy", "server", "stage", "duration", "status", "summary", "data"},
		pgx.CopyFromRows(rows),
	)
	if err == nil {
		// A recurring exception reopens its group if it had been resolved.
		if rerr := s.reopenResolved(ctx, exceptionGroups); rerr != nil {
			s.log.Warn("reopen resolved exceptions failed", "error", rerr)
		}
	}
	return int(n), err
}

// extract pulls the shared hot columns out of a decoded record.
func extract(m map[string]any) Record {
	rec := Record{
		Type:    str(m["t"]),
		TraceID: str(m["trace_id"]),
		Group:   str(m["_group"]),
		UserID:  str(m["user"]),
		Deploy:  str(m["deploy"]),
		Server:  str(m["server"]),
		Stage:   str(m["execution_stage"]),
		TS:      parseTimestamp(m["timestamp"]),
	}
	if m["user"] != nil && rec.UserID == "" {
		// user record carries "id" instead
		rec.UserID = str(m["id"])
	}
	if rec.Type == "user" {
		rec.UserID = str(m["id"])
	}
	rec.Duration = i64(m["duration"])
	rec.Summary = summarize(m)
	switch rec.Type {
	case "request", "outgoing-request":
		rec.Status = str(m["status_code"])
	case "command":
		rec.Status = str(m["exit_code"])
	case "job-attempt", "scheduled-task":
		rec.Status = str(m["status"])
	case "log":
		rec.Status = str(m["level"])
	case "exception":
		if b, ok := m["handled"].(bool); ok {
			if b {
				rec.Status = "handled"
			} else {
				rec.Status = "unhandled"
			}
		}
	case "cache-event":
		rec.Status = str(m["type"])
	case "mail", "notification":
		if b, ok := m["failed"].(bool); ok && b {
			rec.Status = "failed"
		} else {
			rec.Status = "sent"
		}
	}
	return rec
}

// summaryKeys are checked in order; the first non-empty one describes the
// record. summaryMax bounds what lands in the trigram index.
var summaryKeys = []string{"url", "sql", "message", "name", "class", "key", "id"}

const summaryMax = 500

// summarize picks the most descriptive payload field for search and list
// labels. summarySQL below computes the same value for rows that predate
// the column; keep the two in step.
func summarize(m map[string]any) string {
	for _, key := range summaryKeys {
		v := str(m[key])
		if v == "" {
			continue
		}
		if key == "url" {
			if method := str(m["method"]); method != "" {
				v = method + " " + v
			}
		}
		return truncRunes(v, summaryMax)
	}
	return ""
}

// summarySQL is summarize() in SQL, over a records row aliased "r". left()
// counts characters, matching truncRunes.
const summarySQL = `left(coalesce(
	CASE WHEN r.data->>'url' <> '' THEN trim(concat_ws(' ', nullif(r.data->>'method', ''), r.data->>'url')) END,
	nullif(r.data->>'sql', ''), nullif(r.data->>'message', ''), nullif(r.data->>'name', ''),
	nullif(r.data->>'class', ''), nullif(r.data->>'key', ''), nullif(r.data->>'id', ''),
	''), 500)`

// truncRunes cuts to at most n runes, never splitting a multi-byte character.
func truncRunes(s string, n int) string {
	if len(s) <= n { // n bytes is a lower bound on n runes
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func parseTimestamp(v any) time.Time {
	switch t := v.(type) {
	case float64:
		sec := int64(t)
		nsec := int64((t - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC()
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			sec := int64(f)
			return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
		}
	}
	return time.Now().UTC()
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func i64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// BackfillSummaries fills the summary column for rows ingested before it
// existed, walking id-descending in bounded batches so ingest keeps running
// throughout. Rows whose payload has no summarizable field are scanned but
// never written, so repeated runs are read-only once the work is done.
func (s *Store) BackfillSummaries(ctx context.Context) (int64, error) {
	const batch = 5000
	q := fmt.Sprintf(`
		WITH b AS (
			SELECT r.id, %s AS s FROM records r
			WHERE r.id < $1 AND r.summary = ''
			ORDER BY r.id DESC LIMIT %d
		), upd AS (
			UPDATE records t SET summary = b.s FROM b WHERE t.id = b.id AND b.s <> ''
			RETURNING 1
		)
		SELECT coalesce(min(b.id), 0), count(*), (SELECT count(*) FROM upd) FROM b`, summarySQL, batch)

	var total int64
	cursor := int64(math.MaxInt64)
	for {
		var minID, scanned, updated int64
		if err := s.pool.QueryRow(ctx, q, cursor).Scan(&minID, &scanned, &updated); err != nil {
			return total, err
		}
		total += updated
		if scanned == 0 {
			return total, nil
		}
		cursor = minID
	}
}

// Prune deletes records older than the retention window. It deletes in
// bounded batches rather than one statement so a large backlog (e.g. the
// first prune after retention is enabled) never holds a long transaction or
// bloats WAL — each batch commits independently.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	const batch = 10000
	interval := fmt.Sprintf("%d seconds", int64(olderThan.Seconds()))
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM records WHERE id IN (
				SELECT id FROM records WHERE ts < now() - $1::interval LIMIT $2
			)`, interval, batch)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < batch {
			return total, nil
		}
	}
}
