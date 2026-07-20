package store

import (
	"context"
	"fmt"
	"time"
)

// Exception triage state lives in its own table keyed by group hash.
// A group with no row is "open"; resolving or ignoring inserts a row.
// New occurrences of a resolved group delete the row (auto-reopen),
// while ignored groups stay ignored.
const exceptionSchema = `
CREATE TABLE IF NOT EXISTS exception_status (
    group_hash TEXT PRIMARY KEY,
    status     TEXT        NOT NULL CHECK (status IN ('resolved','ignored')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func (s *Store) migrateExceptions(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, exceptionSchema)
	return err
}

// ExceptionGroup is one distinct exception (by _group hash) with its
// occurrence stats, triage status, and details from the latest occurrence.
type ExceptionGroup struct {
	Group     string
	Class     string
	Message   string
	File      string
	Line      string
	Count     int64
	Unhandled int64
	FirstSeen time.Time
	LastSeen  time.Time
	Status    string // open | resolved | ignored
	StatusAt  time.Time
	LastID    int64 // latest record id, for the stack-trace view
}

// ExceptionGroups lists exception groups seen in [since, until), newest
// first. status filters by triage state ("" = all); search matches the
// class or message of the latest occurrence; app/stage scope the listing.
// FirstSeen is global (not clipped to the window) so triage age is accurate.
func (s *Store) ExceptionGroups(ctx context.Context, app, stage string, since, until time.Time, status, search string, limit int) ([]ExceptionGroup, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	end := "now()"
	args := []any{since, app, stage}
	if !until.IsZero() {
		args = append(args, until)
		end = "$4"
	}
	q := fmt.Sprintf(`
		SELECT g.group_hash,
		       coalesce(r.data->>'class', ''),
		       coalesce(r.data->>'message', ''),
		       coalesce(r.data->>'file', ''),
		       coalesce(r.data->>'line', ''),
		       g.cnt, g.unhandled, fs.first_seen, g.last_seen,
		       coalesce(es.status, 'open'),
		       coalesce(es.updated_at, to_timestamp(0)),
		       g.last_id
		FROM (
			SELECT group_hash, count(*) AS cnt,
			       count(*) FILTER (WHERE status = 'unhandled') AS unhandled,
			       max(ts) AS last_seen, max(id) AS last_id
			FROM records
			WHERE type = 'exception' AND group_hash <> '' AND ts >= $1 AND ts < %s
			  AND ($2 = '' OR app = $2) AND ($3 = '' OR stage = $3)
			GROUP BY group_hash
		) g
		JOIN records r ON r.id = g.last_id
		LEFT JOIN exception_status es ON es.group_hash = g.group_hash
		CROSS JOIN LATERAL (
			SELECT min(ts) AS first_seen FROM records
			WHERE type = 'exception' AND group_hash = g.group_hash
			  AND ($2 = '' OR app = $2) AND ($3 = '' OR stage = $3)
		) fs`, end)

	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" WHERE coalesce(es.status, 'open') = $%d", len(args))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		kw := " WHERE "
		if status != "" {
			kw = " AND "
		}
		q += fmt.Sprintf("%sconcat(r.data->>'class', ': ', r.data->>'message') ILIKE $%d", kw, len(args))
	}
	q += fmt.Sprintf(" ORDER BY g.last_seen DESC LIMIT %d", limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExceptionGroup
	for rows.Next() {
		var g ExceptionGroup
		if err := rows.Scan(&g.Group, &g.Class, &g.Message, &g.File, &g.Line,
			&g.Count, &g.Unhandled, &g.FirstSeen, &g.LastSeen, &g.Status, &g.StatusAt, &g.LastID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ExceptionStatusCounts tallies groups per triage state within the window,
// for the Open/Resolved/Ignored tab badges.
func (s *Store) ExceptionStatusCounts(ctx context.Context, app, stage string, since, until time.Time) (map[string]int64, error) {
	end := "now()"
	args := []any{since, app, stage}
	if !until.IsZero() {
		args = append(args, until)
		end = "$4"
	}
	q := fmt.Sprintf(`
		SELECT coalesce(es.status, 'open'), count(*)
		FROM (
			SELECT DISTINCT group_hash FROM records
			WHERE type = 'exception' AND group_hash <> '' AND ts >= $1 AND ts < %s
			  AND ($2 = '' OR app = $2) AND ($3 = '' OR stage = $3)
		) g
		LEFT JOIN exception_status es ON es.group_hash = g.group_hash
		GROUP BY 1`, end)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// GetExceptionGroup returns one group's all-time (within retention) stats,
// scoped to app/stage when non-empty.
func (s *Store) GetExceptionGroup(ctx context.Context, app, stage, group string) (*ExceptionGroup, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT g.group_hash,
		       coalesce(r.data->>'class', ''),
		       coalesce(r.data->>'message', ''),
		       coalesce(r.data->>'file', ''),
		       coalesce(r.data->>'line', ''),
		       g.cnt, g.unhandled, g.first_seen, g.last_seen,
		       coalesce(es.status, 'open'),
		       coalesce(es.updated_at, to_timestamp(0)),
		       g.last_id
		FROM (
			SELECT group_hash, count(*) AS cnt,
			       count(*) FILTER (WHERE status = 'unhandled') AS unhandled,
			       min(ts) AS first_seen, max(ts) AS last_seen, max(id) AS last_id
			FROM records
			WHERE type = 'exception' AND group_hash = $1 AND ($2 = '' OR app = $2) AND ($3 = '' OR stage = $3)
			GROUP BY group_hash
		) g
		JOIN records r ON r.id = g.last_id
		LEFT JOIN exception_status es ON es.group_hash = g.group_hash`, group, app, stage)
	var g ExceptionGroup
	if err := row.Scan(&g.Group, &g.Class, &g.Message, &g.File, &g.Line,
		&g.Count, &g.Unhandled, &g.FirstSeen, &g.LastSeen, &g.Status, &g.StatusAt, &g.LastID); err != nil {
		return nil, err
	}
	return &g, nil
}

// SetExceptionStatus updates a group's triage state. "open" clears the row.
func (s *Store) SetExceptionStatus(ctx context.Context, group, status string) error {
	switch status {
	case "open":
		_, err := s.pool.Exec(ctx, `DELETE FROM exception_status WHERE group_hash = $1`, group)
		return err
	case "resolved", "ignored":
		_, err := s.pool.Exec(ctx, `
			INSERT INTO exception_status (group_hash, status, updated_at) VALUES ($1, $2, now())
			ON CONFLICT (group_hash) DO UPDATE SET status = $2, updated_at = now()`, group, status)
		return err
	default:
		return fmt.Errorf("invalid exception status %q", status)
	}
}

// reopenResolved clears "resolved" for groups that just recurred, so they
// show up as open again. Ignored groups are left alone.
func (s *Store) reopenResolved(ctx context.Context, groups []string) error {
	if len(groups) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM exception_status WHERE status = 'resolved' AND group_hash = ANY($1)`, groups)
	return err
}
