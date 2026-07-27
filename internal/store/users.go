package store

import (
	"context"
	"fmt"
	"time"
)

// UserStat aggregates one user's activity across all record types.
type UserStat struct {
	UserID   string
	Name     string
	Username string
	Events   int64
	Requests int64
	Errors   int64
	LastSeen time.Time
}

// UserStats lists the most recently active users in the window, with
// identity fields taken from each user's latest "user" record.
func (s *Store) UserStats(ctx context.Context, app, stage string, since time.Time, limit int) ([]UserStat, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []any{since, limit}
	scope := optEq(&args, "app", app) + optEq(&args, "stage", stage)
	q := fmt.Sprintf(`
		SELECT r.user_id, coalesce(u.name, ''), coalesce(u.username, ''),
		       r.events, r.requests, r.errors, r.last_seen
		FROM (
			SELECT user_id, count(*) AS events,
			       count(*) FILTER (WHERE type = 'request') AS requests,
			       count(*) FILTER (WHERE %s = 'err') AS errors,
			       max(ts) AS last_seen
			FROM records
			WHERE user_id <> '' AND ts >= $1%s
			GROUP BY user_id
		) r
		LEFT JOIN LATERAL (
			SELECT data->>'name' AS name, data->>'username' AS username
			FROM records
			WHERE type = 'user' AND user_id = r.user_id
			ORDER BY ts DESC LIMIT 1
		) u ON true
		ORDER BY r.last_seen DESC
		LIMIT $2`, statusClassSQL, scope)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserStat
	for rows.Next() {
		var u UserStat
		if err := rows.Scan(&u.UserID, &u.Name, &u.Username,
			&u.Events, &u.Requests, &u.Errors, &u.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserNames maps user IDs to a display label, taken from each user's most
// recent "user" record: its name, or its username when the name is blank.
// IDs with no such record are absent from the map — most records carry only
// a numeric id, so anything showing one can look the person up here.
func (s *Store) UserNames(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (user_id) user_id,
		       coalesce(nullif(data->>'name', ''), nullif(data->>'username', ''), '')
		FROM records
		WHERE type = 'user' AND user_id = ANY($1)
		ORDER BY user_id, ts DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if name != "" {
			out[id] = name
		}
	}
	return out, rows.Err()
}

// UserTypeCounts breaks one user's activity down by record type, most
// frequent first, for the filter chips on the user page.
func (s *Store) UserTypeCounts(ctx context.Context, app, stage, userID string) ([]TypeCount, error) {
	args := []any{userID}
	scope := optEq(&args, "app", app) + optEq(&args, "stage", stage)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT type, count(*) FROM records
		WHERE user_id = $1%s
		GROUP BY type ORDER BY count(*) DESC, type`, scope), args...)
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

// GetUserStat returns one user's all-time (within retention) activity.
func (s *Store) GetUserStat(ctx context.Context, app, stage, userID string) (*UserStat, error) {
	q := fmt.Sprintf(`
		SELECT r.user_id, coalesce(u.name, ''), coalesce(u.username, ''),
		       r.events, r.requests, r.errors, r.last_seen
		FROM (
			SELECT user_id, count(*) AS events,
			       count(*) FILTER (WHERE type = 'request') AS requests,
			       count(*) FILTER (WHERE %s = 'err') AS errors,
			       max(ts) AS last_seen
			FROM records
			WHERE user_id = $1 AND ($2 = '' OR app = $2) AND ($3 = '' OR stage = $3)
			GROUP BY user_id
		) r
		LEFT JOIN LATERAL (
			SELECT data->>'name' AS name, data->>'username' AS username
			FROM records
			WHERE type = 'user' AND user_id = r.user_id
			ORDER BY ts DESC LIMIT 1
		) u ON true`, statusClassSQL)
	var u UserStat
	err := s.pool.QueryRow(ctx, q, userID, app, stage).Scan(&u.UserID, &u.Name, &u.Username,
		&u.Events, &u.Requests, &u.Errors, &u.LastSeen)
	if err != nil {
		return nil, err
	}
	return &u, nil
}
