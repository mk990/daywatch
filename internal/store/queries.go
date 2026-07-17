package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ListFilter narrows record listings.
type ListFilter struct {
	Type    string
	TraceID string
	Group   string
	UserID  string
	Status  string
	Search  string // matched against data::text
	Since   time.Time
	Limit   int
	Offset  int
}

func (f ListFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Type != "" {
		add("type = $%d", f.Type)
	}
	if f.TraceID != "" {
		add("trace_id = $%d", f.TraceID)
	}
	if f.Group != "" {
		add("group_hash = $%d", f.Group)
	}
	if f.UserID != "" {
		add("user_id = $%d", f.UserID)
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Search != "" {
		add("data::text ILIKE $%d", "%"+f.Search+"%")
	}
	if !f.Since.IsZero() {
		add("ts >= $%d", f.Since)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func (s *Store) List(ctx context.Context, f ListFilter) ([]Record, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	where, args := f.where()
	q := fmt.Sprintf(`SELECT id, type, ts, trace_id, group_hash, user_id, deploy, server, stage, duration, status, data
		FROM records %s ORDER BY ts DESC, id DESC LIMIT %d OFFSET %d`, where, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var data []byte
		if err := rows.Scan(&r.ID, &r.Type, &r.TS, &r.TraceID, &r.Group, &r.UserID,
			&r.Deploy, &r.Server, &r.Stage, &r.Duration, &r.Status, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &r.Data); err != nil {
			r.Data = map[string]any{"_error": "unparseable"}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Count(ctx context.Context, f ListFilter) (int64, error) {
	where, args := f.where()
	var n int64
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM records "+where, args...).Scan(&n)
	return n, err
}

func (s *Store) Get(ctx context.Context, id int64) (*Record, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, type, ts, trace_id, group_hash, user_id, deploy, server, stage, duration, status, data
		FROM records WHERE id = $1`, id)
	var r Record
	var data []byte
	if err := row.Scan(&r.ID, &r.Type, &r.TS, &r.TraceID, &r.Group, &r.UserID,
		&r.Deploy, &r.Server, &r.Stage, &r.Duration, &r.Status, &data); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.Data); err != nil {
		r.Data = map[string]any{"_error": "unparseable"}
	}
	return &r, nil
}

// TypeCount is a per-type tally for the dashboard.
type TypeCount struct {
	Type  string
	Count int64
}

func (s *Store) CountsByType(ctx context.Context, since time.Time) ([]TypeCount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT type, count(*) FROM records WHERE ts >= $1 GROUP BY type ORDER BY count(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TypeCount
	for rows.Next() {
		var tc TypeCount
		if err := rows.Scan(&tc.Type, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// RouteStat aggregates request performance per route group.
type RouteStat struct {
	Group    string
	Label    string
	Count    int64
	AvgMs    float64
	P95Ms    float64
	MaxMs    int64
	Errors   int64
	LastSeen time.Time
}

func (s *Store) RequestStats(ctx context.Context, since time.Time, limit int) ([]RouteStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT group_hash,
		       max(concat(data->>'method', ' ', coalesce(nullif(data->>'route_path',''), data->>'url'))) AS label,
		       count(*),
		       avg(duration)::float8,
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration)::float8,
		       max(duration),
		       count(*) FILTER (WHERE status >= '500' AND status < '600' AND length(status) = 3),
		       max(ts)
		FROM records
		WHERE type = 'request' AND ts >= $1
		GROUP BY group_hash
		ORDER BY count(*) DESC
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteStat
	for rows.Next() {
		var rs RouteStat
		if err := rows.Scan(&rs.Group, &rs.Label, &rs.Count, &rs.AvgMs, &rs.P95Ms, &rs.MaxMs, &rs.Errors, &rs.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// GroupStat is a generic per-group aggregate used for queries, exceptions, etc.
type GroupStat struct {
	Group    string
	Label    string
	Count    int64
	AvgMs    float64
	MaxMs    int64
	LastSeen time.Time
}

func (s *Store) GroupStats(ctx context.Context, typ, labelExpr string, since time.Time, limit int) ([]GroupStat, error) {
	q := fmt.Sprintf(`
		SELECT group_hash, max(%s) AS label, count(*), avg(duration)::float8, max(duration), max(ts)
		FROM records
		WHERE type = $1 AND ts >= $2 AND group_hash <> ''
		GROUP BY group_hash
		ORDER BY count(*) DESC
		LIMIT $3`, labelExpr)
	rows, err := s.pool.Query(ctx, q, typ, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupStat
	for rows.Next() {
		var gs GroupStat
		if err := rows.Scan(&gs.Group, &gs.Label, &gs.Count, &gs.AvgMs, &gs.MaxMs, &gs.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, gs)
	}
	return out, rows.Err()
}

// TimeBucket is one point of a time-series chart.
type TimeBucket struct {
	Bucket time.Time
	Count  int64
	AvgMs  float64
}

func (s *Store) Timeline(ctx context.Context, typ string, since time.Time, bucketMinutes int) ([]TimeBucket, error) {
	q := `
		SELECT date_bin($3::interval, ts, $2) AS bucket, count(*), coalesce(avg(duration),0)::float8
		FROM records
		WHERE ts >= $2 AND ($1 = '' OR type = $1)
		GROUP BY bucket
		ORDER BY bucket`
	rows, err := s.pool.Query(ctx, q, typ, since.Truncate(time.Minute), fmt.Sprintf("%d minutes", bucketMinutes))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeBucket
	for rows.Next() {
		var tb TimeBucket
		if err := rows.Scan(&tb.Bucket, &tb.Count, &tb.AvgMs); err != nil {
			return nil, err
		}
		out = append(out, tb)
	}
	return out, rows.Err()
}
