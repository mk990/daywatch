package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/zeebo/xxh3"
)

// Config holds all runtime settings, sourced from environment variables.
type Config struct {
	// IngestAddr is the TCP address the Nightwatch-protocol listener binds to.
	IngestAddr string
	// HTTPAddr is the address the web panel binds to.
	HTTPAddr string
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// Token is the shared NIGHTWATCH_TOKEN. When empty, any token hash is accepted.
	Token string
	// TokenHash is the first 7 hex chars of xxh128(Token), matching the PHP agent.
	TokenHash string
	// RetentionDays: records older than this are pruned. 0 disables pruning.
	RetentionDays int
	// ReadTimeout for ingest connections.
	ReadTimeout time.Duration
	// Username/Password protect the web panel. Both empty disables auth.
	Username string
	Password string
	// JWTSecret signs session tokens. Derived from credentials when unset.
	JWTSecret []byte
	// BaseURL is the public panel URL included in alert notifications.
	BaseURL string
}

func FromEnv() (*Config, error) {
	cfg := &Config{
		IngestAddr:    getenv("DW_INGEST_ADDR", ":2407"),
		HTTPAddr:      getenv("DW_HTTP_ADDR", ":8080"),
		DatabaseURL:   getenv("DATABASE_URL", ""),
		Token:         getenv("NIGHTWATCH_TOKEN", ""),
		RetentionDays: 14,
		ReadTimeout:   10 * time.Second,
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	if v := os.Getenv("DW_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid DW_RETENTION_DAYS %q: %w", v, err)
		}
		cfg.RetentionDays = n
	}

	if cfg.Token != "" {
		cfg.TokenHash = TokenHash(cfg.Token)
	}

	cfg.BaseURL = os.Getenv("DW_BASE_URL")
	cfg.Username = os.Getenv("DAYWATCH_USERNAME")
	cfg.Password = os.Getenv("DAYWATCH_PASSWORD")
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, fmt.Errorf("DAYWATCH_USERNAME and DAYWATCH_PASSWORD must be set together")
	}
	if secret := os.Getenv("DW_JWT_SECRET"); secret != "" {
		cfg.JWTSecret = []byte(secret)
	} else if cfg.Username != "" {
		// Stable across restarts so sessions survive redeploys.
		sum := sha256.Sum256([]byte("daywatch-jwt:" + cfg.Username + ":" + cfg.Password + ":" + cfg.Token))
		cfg.JWTSecret = sum[:]
	}

	return cfg, nil
}

// TokenHash mirrors Laravel Nightwatch: substr(hash('xxh128', $token), 0, 7).
func TokenHash(token string) string {
	sum := xxh3.Hash128([]byte(token)).Bytes()
	return hex.EncodeToString(sum[:])[:7]
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
