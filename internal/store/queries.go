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
	App     string
	Stage   string
	Type    string
	TraceID string
	Group   string
	UserID  string
	Status  string
	Search  string // matched against data::text
	Since   time.Time
	Until   time.Time
	// SortSlowest orders by duration descending instead of newest first.
	SortSlowest bool
	Limit       int
	Offset      int
}

func (f ListFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.App != "" {
		add("app = $%d", f.App)
	}
	if f.Stage != "" {
		add("stage = $%d", f.Stage)
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
	if !f.Until.IsZero() {
		add("ts < $%d", f.Until)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// optEq returns " AND col = $n" (appending val to args) when val is non-empty,
// and "" otherwise. Building the condition only when needed — instead of
// "($n = '' OR col = $n)" — matters under pgx's prepared statements: a generic
// plan cannot fold "$n = ''" into a constant, so the OR form blocks partial
// indexes like records_app_idx and is evaluated per row.
func optEq(args *[]any, col, val string) string {
	if val == "" {
		return ""
	}
	*args = append(*args, val)
	return fmt.Sprintf(" AND %s = $%d", col, len(*args))
}

func (s *Store) List(ctx context.Context, f ListFilter) ([]Record, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	where, args := f.where()
	orderBy := "ts DESC, id DESC"
	if f.SortSlowest {
		orderBy = "duration DESC, id DESC"
	}
	q := fmt.Sprintf(`SELECT id, app, type, ts, trace_id, group_hash, user_id, deploy, server, stage, duration, status, data
		FROM records %s ORDER BY %s LIMIT %d OFFSET %d`, where, orderBy, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var data []byte
		if err := rows.Scan(&r.ID, &r.App, &r.Type, &r.TS, &r.TraceID, &r.Group, &r.UserID,
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

// CountCap bounds Count: exact totals are only computed up to this many rows,
// so pagination never walks millions of index entries per page render.
const CountCap = 50000

// Count returns the number of matching records, capped at CountCap+1
// (meaning "more than CountCap").
func (s *Store) Count(ctx context.Context, f ListFilter) (int64, error) {
	where, args := f.where()
	q := fmt.Sprintf("SELECT count(*) FROM (SELECT 1 FROM records %s LIMIT %d) c", where, CountCap+1)
	var n int64
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

func (s *Store) Get(ctx context.Context, id int64) (*Record, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, app, type, ts, trace_id, group_hash, user_id, deploy, server, stage, duration, status, data
		FROM records WHERE id = $1`, id)
	var r Record
	var data []byte
	if err := row.Scan(&r.ID, &r.App, &r.Type, &r.TS, &r.TraceID, &r.Group, &r.UserID,
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

func (s *Store) CountsByType(ctx context.Context, app, stage string, since time.Time) ([]TypeCount, error) {
	// Full hours are summed from the hourly rollups (tiny table); only the
	// current partial hour is counted live from raw records. This keeps the
	// dashboard O(rollup rows) instead of scanning every record since `since`.
	args := []any{since.Truncate(time.Hour)}
	appCond := optEq(&args, "app", app)
	stageCond := optEq(&args, "stage", stage)
	q := fmt.Sprintf(`
		SELECT type, sum(cnt) FROM (
			SELECT type, cnt FROM rollups_hourly
			WHERE bucket >= $1 AND bucket < date_trunc('hour', now())%[1]s%[2]s
			UNION ALL
			SELECT type, 1 FROM records
			WHERE ts >= greatest($1, date_trunc('hour', now()))%[1]s%[2]s
		) t GROUP BY type ORDER BY sum(cnt) DESC`, appCond, stageCond)
	rows, err := s.pool.Query(ctx, q, args...)
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

func (s *Store) RequestStats(ctx context.Context, app, stage string, since time.Time, limit int) ([]RouteStat, error) {
	args := []any{since, limit}
	scope := optEq(&args, "app", app) + optEq(&args, "stage", stage)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT group_hash,
		       max(concat(data->>'method', ' ', coalesce(nullif(data->>'route_path',''), data->>'url'))) AS label,
		       count(*),
		       avg(duration)::float8,
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration)::float8,
		       max(duration),
		       count(*) FILTER (WHERE status >= '500' AND status < '600' AND length(status) = 3),
		       max(ts)
		FROM records
		WHERE type = 'request' AND ts >= $1%s
		GROUP BY group_hash
		ORDER BY count(*) DESC
		LIMIT $2`, scope), args...)
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

// GroupStats aggregates per group_hash. orderBy picks the ranking:
// "count" (most frequent), "avg" or "max" (slowest), "total" (most time overall).
func (s *Store) GroupStats(ctx context.Context, app, stage, typ, labelExpr, orderBy string, since time.Time, limit int) ([]GroupStat, error) {
	orderExpr := map[string]string{
		"count": "count(*)",
		"avg":   "avg(duration)",
		"max":   "max(duration)",
		"total": "sum(duration)",
	}[orderBy]
	if orderExpr == "" {
		orderExpr = "count(*)"
	}
	args := []any{typ, since, limit}
	scope := optEq(&args, "app", app) + optEq(&args, "stage", stage)
	q := fmt.Sprintf(`
		SELECT group_hash, max(%s) AS label, count(*), avg(duration)::float8, max(duration), max(ts)
		FROM records
		WHERE type = $1 AND ts >= $2 AND group_hash <> ''%s
		GROUP BY group_hash
		ORDER BY %s DESC
		LIMIT $3`, labelExpr, scope, orderExpr)
	rows, err := s.pool.Query(ctx, q, args...)
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

// StageNames returns the distinct execution stages (environments) seen so
// far, alphabetically. Historical stages come from the small rollups table;
// the current partial hour is checked live so a brand-new stage shows up
// without waiting for the rollup ticker.
func (s *Store) StageNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT stage FROM rollups_hourly WHERE stage <> ''
		UNION
		SELECT DISTINCT stage FROM records
		WHERE ts >= date_trunc('hour', now()) AND stage <> ''
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// statusClassSQL buckets the status column into ok/warn/err/other,
// mirroring the badge colors used across the panel.
const statusClassSQL = `CASE
	WHEN status ~ '^[23][0-9][0-9]$' OR status IN ('0','sent','processed','handled','hit','success','info','debug') THEN 'ok'
	WHEN status ~ '^4[0-9][0-9]$' OR status IN ('warning','miss','released','notice') THEN 'warn'
	WHEN status ~ '^5[0-9][0-9]$' OR status IN ('failed','unhandled','error','critical','emergency','alert') THEN 'err'
	ELSE 'other'
END`

// ClassBucket is one time bucket split by status class.
type ClassBucket struct {
	Bucket time.Time
	OK     int64
	Warn   int64
	Err    int64
	Other  int64
	AvgMs  float64
	P95    float64
	P99    float64
}

// TimelineByClass returns a gap-free series of buckets from since until
// `until` (or now when zero), with counts split by status class.
// typ = "" aggregates all record types; app/stage = "" aggregate everything.
func (s *Store) TimelineByClass(ctx context.Context, app, stage, typ string, since, until time.Time, bucketMinutes int) ([]ClassBucket, error) {
	origin := since.Truncate(time.Minute)
	end := time.Now()
	if !until.IsZero() {
		end = until
	}
	// Hour-or-larger buckets are served from pre-aggregated hourly rollups,
	// which stay fast on long ranges and outlive raw-record pruning.
	if bucketMinutes >= 60 && bucketMinutes%60 == 0 {
		return s.timelineFromRollups(ctx, app, stage, typ, since.Truncate(time.Hour), end, bucketMinutes)
	}
	args := []any{origin, fmt.Sprintf("%d minutes", bucketMinutes), end}
	scope := optEq(&args, "type", typ) + optEq(&args, "app", app) + optEq(&args, "stage", stage)
	q := fmt.Sprintf(`
		SELECT date_bin($2::interval, ts, $1) AS bucket,
		       count(*) FILTER (WHERE %[1]s = 'ok'),
		       count(*) FILTER (WHERE %[1]s = 'warn'),
		       count(*) FILTER (WHERE %[1]s = 'err'),
		       count(*) FILTER (WHERE %[1]s = 'other'),
		       coalesce(avg(duration), 0)::float8,
		       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration), 0)::float8,
		       coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY duration), 0)::float8
		FROM records
		WHERE ts >= $1 AND ts < $3%[2]s
		GROUP BY bucket
		ORDER BY bucket`, statusClassSQL, scope)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byBucket := map[int64]ClassBucket{}
	for rows.Next() {
		var cb ClassBucket
		if err := rows.Scan(&cb.Bucket, &cb.OK, &cb.Warn, &cb.Err, &cb.Other, &cb.AvgMs, &cb.P95, &cb.P99); err != nil {
			return nil, err
		}
		byBucket[cb.Bucket.Unix()] = cb
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fill gaps so the chart shows a continuous series.
	step := time.Duration(bucketMinutes) * time.Minute
	var out []ClassBucket
	for t := origin; t.Before(end); t = t.Add(step) {
		if cb, ok := byBucket[t.Unix()]; ok {
			out = append(out, cb)
		} else {
			out = append(out, ClassBucket{Bucket: t})
		}
	}
	return out, nil
}
