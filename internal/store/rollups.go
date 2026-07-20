package store

import (
	"context"
	"fmt"
	"time"
)

// Hourly rollups pre-aggregate records per (hour, app, type, stage) so
// long-range charts stay fast and survive raw-record pruning. They are
// upserted by a background ticker (full backfill at startup) and read by
// TimelineByClass for bucket sizes of an hour or more; the current partial
// hour is always aggregated live from records.
const rollupSchema = `
CREATE TABLE IF NOT EXISTS rollups_hourly (
    bucket         TIMESTAMPTZ NOT NULL,
    app            TEXT        NOT NULL DEFAULT '',
    type           TEXT        NOT NULL,
    stage          TEXT        NOT NULL DEFAULT '',
    ok             BIGINT      NOT NULL DEFAULT 0,
    warn           BIGINT      NOT NULL DEFAULT 0,
    err            BIGINT      NOT NULL DEFAULT 0,
    other          BIGINT      NOT NULL DEFAULT 0,
    cnt            BIGINT      NOT NULL DEFAULT 0,
    total_duration BIGINT      NOT NULL DEFAULT 0,
    p95            BIGINT      NOT NULL DEFAULT 0,
    p99            BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, app, type, stage)
);
`

func (s *Store) migrateRollups(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, rollupSchema); err != nil {
		return err
	}
	// Pre-stage installs have a (bucket, app, type) primary key. Rebuild it
	// with the stage column and drop every row the startup backfill can
	// recompute from raw records; rows older than retention are kept as
	// stage='' aggregates (they still count toward unfiltered charts, but a
	// stage filter cannot see them).
	var hasStage bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name = 'rollups_hourly' AND column_name = 'stage')`).Scan(&hasStage); err != nil {
		return err
	}
	if hasStage {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		ALTER TABLE rollups_hourly ADD COLUMN stage TEXT NOT NULL DEFAULT '';
		ALTER TABLE rollups_hourly DROP CONSTRAINT rollups_hourly_pkey;
		ALTER TABLE rollups_hourly ADD PRIMARY KEY (bucket, app, type, stage);
		DELETE FROM rollups_hourly WHERE bucket >= coalesce(
			(SELECT date_trunc('hour', min(ts)) FROM records), 'infinity'::timestamptz);`)
	return err
}

// UpdateRollups (re)computes hourly rollups for records at or after since.
// Pass the zero time for a full backfill.
func (s *Store) UpdateRollups(ctx context.Context, since time.Time) error {
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	q := fmt.Sprintf(`
		INSERT INTO rollups_hourly
			(bucket, app, type, stage, ok, warn, err, other, cnt, total_duration, p95, p99)
		SELECT date_trunc('hour', ts), app, type, stage,
		       count(*) FILTER (WHERE %[1]s = 'ok'),
		       count(*) FILTER (WHERE %[1]s = 'warn'),
		       count(*) FILTER (WHERE %[1]s = 'err'),
		       count(*) FILTER (WHERE %[1]s = 'other'),
		       count(*),
		       coalesce(sum(duration), 0),
		       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration), 0)::bigint,
		       coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY duration), 0)::bigint
		FROM records
		WHERE ts >= date_trunc('hour', $1::timestamptz)
		GROUP BY 1, 2, 3, 4
		ON CONFLICT (bucket, app, type, stage) DO UPDATE SET
			ok = EXCLUDED.ok, warn = EXCLUDED.warn, err = EXCLUDED.err,
			other = EXCLUDED.other, cnt = EXCLUDED.cnt,
			total_duration = EXCLUDED.total_duration,
			p95 = EXCLUDED.p95, p99 = EXCLUDED.p99`, statusClassSQL)
	_, err := s.pool.Exec(ctx, q, since)
	return err
}

// PruneRollups deletes rollups older than the retention window.
func (s *Store) PruneRollups(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM rollups_hourly WHERE bucket < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// timelineFromRollups serves TimelineByClass for hour-or-larger buckets:
// full hours come from rollups (weighted-average percentiles), the current
// partial hour is aggregated live from records so charts stay fresh.
func (s *Store) timelineFromRollups(ctx context.Context, app, stage, typ string, origin, end time.Time, bucketMinutes int) ([]ClassBucket, error) {
	q := fmt.Sprintf(`
		WITH agg AS (
			SELECT date_bin($3::interval, bucket, $2) AS b,
			       ok, warn, err, other, cnt, total_duration,
			       p95 * cnt AS wp95, p99 * cnt AS wp99
			FROM rollups_hourly
			WHERE bucket >= $2 AND bucket < $4 AND bucket < date_trunc('hour', now())
			  AND ($1 = '' OR type = $1) AND ($5 = '' OR app = $5) AND ($6 = '' OR stage = $6)
			UNION ALL
			SELECT date_bin($3::interval, ts, $2),
			       count(*) FILTER (WHERE %[1]s = 'ok'),
			       count(*) FILTER (WHERE %[1]s = 'warn'),
			       count(*) FILTER (WHERE %[1]s = 'err'),
			       count(*) FILTER (WHERE %[1]s = 'other'),
			       count(*),
			       coalesce(sum(duration), 0),
			       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration), 0) * count(*),
			       coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY duration), 0) * count(*)
			FROM records
			WHERE ts >= greatest($2, date_trunc('hour', now())) AND ts < $4
			  AND ($1 = '' OR type = $1) AND ($5 = '' OR app = $5) AND ($6 = '' OR stage = $6)
			GROUP BY 1
		)
		SELECT b, sum(ok), sum(warn), sum(err), sum(other),
		       coalesce(sum(total_duration)::float8 / nullif(sum(cnt), 0), 0),
		       coalesce(sum(wp95)::float8 / nullif(sum(cnt), 0), 0),
		       coalesce(sum(wp99)::float8 / nullif(sum(cnt), 0), 0)
		FROM agg GROUP BY b ORDER BY b`, statusClassSQL)
	rows, err := s.pool.Query(ctx, q, typ, origin,
		fmt.Sprintf("%d minutes", bucketMinutes), end, app, stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byBucket := map[int64]ClassBucket{}
	for rows.Next() {
		var cb ClassBucket
		if err := rows.Scan(&cb.Bucket, &cb.OK, &cb.Warn, &cb.Err, &cb.Other,
			&cb.AvgMs, &cb.P95, &cb.P99); err != nil {
			return nil, err
		}
		byBucket[cb.Bucket.Unix()] = cb
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

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
