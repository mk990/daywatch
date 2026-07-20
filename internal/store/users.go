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
	q := fmt.Sprintf(`
		SELECT r.user_id, coalesce(u.name, ''), coalesce(u.username, ''),
		       r.events, r.requests, r.errors, r.last_seen
		FROM (
			SELECT user_id, count(*) AS events,
			       count(*) FILTER (WHERE type = 'request') AS requests,
			       count(*) FILTER (WHERE %s = 'err') AS errors,
			       max(ts) AS last_seen
			FROM records
			WHERE user_id <> '' AND ts >= $1 AND ($2 = '' OR app = $2) AND ($4 = '' OR stage = $4)
			GROUP BY user_id
		) r
		LEFT JOIN LATERAL (
			SELECT data->>'name' AS name, data->>'username' AS username
			FROM records
			WHERE type = 'user' AND user_id = r.user_id
			ORDER BY ts DESC LIMIT 1
		) u ON true
		ORDER BY r.last_seen DESC
		LIMIT $3`, statusClassSQL)
	rows, err := s.pool.Query(ctx, q, since, app, limit, stage)
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
