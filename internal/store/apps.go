package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// Registered apps live in the database so they can be managed from the
// panel. The ingest server resolves incoming token hashes against this
// table on every frame, so new apps work without a restart.
const appSchema = `
CREATE TABLE IF NOT EXISTS apps (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    token      TEXT        NOT NULL,
    token_hash TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func (s *Store) migrateApps(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, appSchema)
	return err
}

// App is one registered application with its ingest token.
type App struct {
	ID        int64
	Name      string
	Token     string
	TokenHash string
	CreatedAt time.Time
	// Stats joined in for the panel listing.
	Records  int64
	LastSeen time.Time
}

var appNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

// ValidAppName reports whether a name is acceptable: short, URL-safe.
func ValidAppName(name string) bool { return appNameRe.MatchString(name) }

// GenerateAppToken returns a fresh random ingest token.
func GenerateAppToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "dw_" + hex.EncodeToString(buf), nil
}

// CreateApp registers an app. Fails if the name or token collides.
func (s *Store) CreateApp(ctx context.Context, name, token, tokenHash string) error {
	if !ValidAppName(name) {
		return fmt.Errorf("invalid app name %q: use 1-40 letters, digits, - or _", name)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO apps (name, token, token_hash) VALUES ($1, $2, $3)`,
		name, token, tokenHash)
	return err
}

// SeedApp inserts an env-configured app only if the name is free; the
// panel stays the source of truth after first boot.
func (s *Store) SeedApp(ctx context.Context, name, token, tokenHash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO apps (name, token, token_hash) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO NOTHING`, name, token, tokenHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListApps returns all apps with per-app record stats.
func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, a.token, a.token_hash, a.created_at,
		       coalesce(r.cnt, 0), coalesce(r.last_seen, to_timestamp(0))
		FROM apps a
		LEFT JOIN (
			SELECT app, count(*) AS cnt, max(ts) AS last_seen
			FROM records GROUP BY app
		) r ON r.app = a.name
		ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Token, &a.TokenHash, &a.CreatedAt,
			&a.Records, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AppNames returns registered app names, alphabetically.
func (s *Store) AppNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) GetApp(ctx context.Context, id int64) (*App, error) {
	var a App
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, token, token_hash, created_at FROM apps WHERE id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Token, &a.TokenHash, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateAppToken swaps an app's ingest token (rotation).
func (s *Store) UpdateAppToken(ctx context.Context, id int64, token, tokenHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE apps SET token = $2, token_hash = $3 WHERE id = $1`, id, token, tokenHash)
	return err
}

// DeleteApp unregisters an app. Its records are kept and remain visible
// under "All apps".
func (s *Store) DeleteApp(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, id)
	return err
}

// ResolveApp maps an incoming token hash to an app name, in one query:
// found=false with anyApps=false means no apps are registered at all
// (open ingest); found=false with anyApps=true means the token is invalid.
func (s *Store) ResolveApp(ctx context.Context, tokenHash string) (name string, found, anyApps bool, err error) {
	var n *string
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT name FROM apps WHERE token_hash = $1),
		       EXISTS(SELECT 1 FROM apps)`, tokenHash).Scan(&n, &anyApps)
	if err != nil {
		return "", false, false, err
	}
	if n != nil {
		return *n, true, anyApps, nil
	}
	return "", false, anyApps, nil
}
