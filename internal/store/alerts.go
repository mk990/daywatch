package store

import (
	"context"
	"fmt"
	"time"
)

const alertSchema = `
CREATE TABLE IF NOT EXISTS alert_rules (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT        NOT NULL,
    enabled          BOOLEAN     NOT NULL DEFAULT true,
    record_type      TEXT        NOT NULL DEFAULT '',
    status_class     TEXT        NOT NULL DEFAULT 'err',
    threshold        INT         NOT NULL DEFAULT 1,
    window_minutes   INT         NOT NULL DEFAULT 5,
    cooldown_minutes INT         NOT NULL DEFAULT 15,
    channel_url      TEXT        NOT NULL,
    channel_format   TEXT        NOT NULL DEFAULT 'json',
    telegram_chat_id TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS alert_events (
    id        BIGSERIAL PRIMARY KEY,
    rule_id   BIGINT      NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    fired_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    matched   BIGINT      NOT NULL,
    message   TEXT        NOT NULL,
    delivered BOOLEAN     NOT NULL,
    error     TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alert_events_rule_idx ON alert_events (rule_id, fired_at DESC);
CREATE INDEX IF NOT EXISTS alert_events_fired_idx ON alert_events (fired_at DESC);
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS app TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'threshold';
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS auth_user TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS auth_pass TEXT NOT NULL DEFAULT '';
`

// AlertRule fires a webhook when matching records exceed a threshold
// within a sliding time window.
type AlertRule struct {
	ID              int64
	Name            string
	Enabled         bool
	Kind            string // "threshold" or "new-exception"
	App             string // "" = any app
	RecordType      string // "" = any type
	StatusClass     string // "err", "warn", or "" = any record
	Threshold       int
	WindowMinutes   int
	CooldownMinutes int
	ChannelURL      string
	ChannelFormat   string // json | slack | discord | telegram | ntfy
	TelegramChatID  string
	// AuthUser/AuthPass authenticate against the channel endpoint: both set
	// means HTTP basic auth, AuthPass alone is sent as a bearer token (ntfy
	// access tokens). Both empty sends no Authorization header.
	AuthUser  string
	AuthPass  string
	CreatedAt time.Time
}

// AlertEvent records one firing of a rule.
type AlertEvent struct {
	ID        int64
	RuleID    int64
	RuleName  string
	FiredAt   time.Time
	Matched   int64
	Message   string
	Delivered bool
	Error     string
}

func (s *Store) migrateAlerts(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, alertSchema)
	return err
}

func (s *Store) CreateAlertRule(ctx context.Context, r AlertRule) error {
	if r.Kind == "" {
		r.Kind = "threshold"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO alert_rules
			(name, enabled, kind, app, record_type, status_class, threshold, window_minutes,
			 cooldown_minutes, channel_url, channel_format, telegram_chat_id, auth_user, auth_pass)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		r.Name, r.Enabled, r.Kind, r.App, r.RecordType, r.StatusClass, r.Threshold, r.WindowMinutes,
		r.CooldownMinutes, r.ChannelURL, r.ChannelFormat, r.TelegramChatID, r.AuthUser, r.AuthPass)
	return err
}

func (s *Store) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, enabled, kind, app, record_type, status_class, threshold, window_minutes,
		       cooldown_minutes, channel_url, channel_format, telegram_chat_id,
		       auth_user, auth_pass, created_at
		FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		var r AlertRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.Kind, &r.App, &r.RecordType, &r.StatusClass,
			&r.Threshold, &r.WindowMinutes, &r.CooldownMinutes, &r.ChannelURL,
			&r.ChannelFormat, &r.TelegramChatID, &r.AuthUser, &r.AuthPass, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetAlertRule(ctx context.Context, id int64) (*AlertRule, error) {
	var r AlertRule
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, enabled, kind, app, record_type, status_class, threshold, window_minutes,
		       cooldown_minutes, channel_url, channel_format, telegram_chat_id,
		       auth_user, auth_pass, created_at
		FROM alert_rules WHERE id = $1`, id).
		Scan(&r.ID, &r.Name, &r.Enabled, &r.Kind, &r.App, &r.RecordType, &r.StatusClass,
			&r.Threshold, &r.WindowMinutes, &r.CooldownMinutes, &r.ChannelURL,
			&r.ChannelFormat, &r.TelegramChatID, &r.AuthUser, &r.AuthPass, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateAlertRule rewrites an existing rule. Enabled is left alone — pausing
// and resuming go through ToggleAlertRule — and the firing history is kept,
// so an edited rule stays within its current cooldown.
func (s *Store) UpdateAlertRule(ctx context.Context, r AlertRule) error {
	if r.Kind == "" {
		r.Kind = "threshold"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE alert_rules SET
			name = $2, kind = $3, app = $4, record_type = $5, status_class = $6,
			threshold = $7, window_minutes = $8, cooldown_minutes = $9,
			channel_url = $10, channel_format = $11, telegram_chat_id = $12,
			auth_user = $13, auth_pass = $14
		WHERE id = $1`,
		r.ID, r.Name, r.Kind, r.App, r.RecordType, r.StatusClass, r.Threshold, r.WindowMinutes,
		r.CooldownMinutes, r.ChannelURL, r.ChannelFormat, r.TelegramChatID, r.AuthUser, r.AuthPass)
	return err
}

func (s *Store) ToggleAlertRule(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE alert_rules SET enabled = NOT enabled WHERE id = $1`, id)
	return err
}

func (s *Store) DeleteAlertRule(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1`, id)
	return err
}

// CountMatching counts what a rule matches since the given time. For
// threshold rules that is records matching the type/class filter; for
// new-exception rules it is exception groups whose very first occurrence
// (within retention) falls inside the window.
func (s *Store) CountMatching(ctx context.Context, r AlertRule, since time.Time) (int64, error) {
	var q string
	var args []any
	if r.Kind == "new-exception" {
		q = `
		SELECT count(*) FROM (
			SELECT group_hash FROM records
			WHERE type = 'exception' AND group_hash <> '' AND ($2 = '' OR app = $2)
			GROUP BY group_hash
			HAVING min(ts) >= $1
		) new_groups`
		args = []any{since, r.App}
	} else {
		q = fmt.Sprintf(`
		SELECT count(*) FROM records
		WHERE ts >= $1
		  AND ($2 = '' OR type = $2)
		  AND ($3 = '' OR %s = $3)
		  AND ($4 = '' OR app = $4)`, statusClassSQL)
		args = []any{since, r.RecordType, r.StatusClass, r.App}
	}
	var n int64
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

// RuleState summarizes a rule's firing history. The cooldown runs from the
// last *delivered* notification, not the last attempt: an alert nobody
// received should not silence the rule. Failures counts the undelivered
// firings since that success, so the evaluator can back off an endpoint
// that is down for good.
type RuleState struct {
	LastDelivered time.Time
	LastAttempt   time.Time
	Failures      int
}

func (s *Store) RuleState(ctx context.Context, ruleID int64) (RuleState, error) {
	var st RuleState
	var delivered, attempt *time.Time
	err := s.pool.QueryRow(ctx, `
		WITH ok AS (
			SELECT max(fired_at) AS at FROM alert_events WHERE rule_id = $1 AND delivered
		)
		SELECT ok.at,
		       (SELECT max(fired_at) FROM alert_events WHERE rule_id = $1),
		       (SELECT count(*) FROM alert_events e
		         WHERE e.rule_id = $1 AND NOT e.delivered
		           AND (ok.at IS NULL OR e.fired_at > ok.at))
		FROM ok`, ruleID).Scan(&delivered, &attempt, &st.Failures)
	if err != nil {
		return RuleState{}, err
	}
	if delivered != nil {
		st.LastDelivered = *delivered
	}
	if attempt != nil {
		st.LastAttempt = *attempt
	}
	return st, nil
}

func (s *Store) InsertAlertEvent(ctx context.Context, e AlertEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO alert_events (rule_id, fired_at, matched, message, delivered, error)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.RuleID, e.FiredAt, e.Matched, e.Message, e.Delivered, e.Error)
	return err
}

func (s *Store) ListAlertEvents(ctx context.Context, limit int) ([]AlertEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.rule_id, coalesce(r.name, '(deleted rule)'), e.fired_at,
		       e.matched, e.message, e.delivered, e.error
		FROM alert_events e
		LEFT JOIN alert_rules r ON r.id = e.rule_id
		ORDER BY e.fired_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var e AlertEvent
		if err := rows.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.FiredAt,
			&e.Matched, &e.Message, &e.Delivered, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
