package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mk/daywatch/internal/alert"
	"github.com/mk/daywatch/internal/config"
	"github.com/mk/daywatch/internal/ingest"
	"github.com/mk/daywatch/internal/store"
	"github.com/mk/daywatch/internal/web"
)

// notifyingSink stores records and then signals the live-update hub.
type notifyingSink struct {
	store *store.Store
	hub   *web.Hub
}

func (s *notifyingSink) InsertRecords(ctx context.Context, records []json.RawMessage, app string) (int, error) {
	n, err := s.store.InsertRecords(ctx, records, app)
	if err == nil && n > 0 {
		s.hub.Notify()
	}
	return n, err
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config error", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL, log)
	if err != nil {
		log.Error("store init failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	log.Info("database ready")

	// Env-configured apps seed the database on first boot; after that the
	// panel's Apps page is the source of truth.
	for _, a := range cfg.Apps {
		seeded, err := st.SeedApp(ctx, a.Name, a.Token, a.Hash)
		if err != nil {
			log.Warn("seeding app from env failed", "app", a.Name, "error", err)
		} else if seeded {
			log.Info("app seeded from env", "app", a.Name, "token_hash", a.Hash)
		}
	}

	panel, err := web.New(st, log, web.AuthConfig{
		Username: cfg.Username,
		Password: cfg.Password,
		Secret:   cfg.JWTSecret,
	})
	if err != nil {
		log.Error("web init failed", "error", err)
		os.Exit(1)
	}

	evaluator := alert.New(st, log, cfg.BaseURL, panel.Hub())
	panel.SetAlertTester(evaluator)

	// Wrap the store so every successful ingest wakes live-reload clients.
	sink := &notifyingSink{store: st, hub: panel.Hub()}
	ing := ingest.New(cfg.IngestAddr, st, cfg.ReadTimeout, sink, log)
	if err := ing.Listen(); err != nil {
		log.Error("ingest listen failed", "error", err)
		os.Exit(1)
	}

	if names, err := st.AppNames(ctx); err == nil && len(names) == 0 {
		log.Warn("no apps registered: accepting any token (create apps in the panel or set DW_APPS)")
	}
	if cfg.Username != "" {
		log.Info("panel authentication enabled", "username", cfg.Username)
	} else {
		log.Warn("DAYWATCH_USERNAME/DAYWATCH_PASSWORD not set: panel is unprotected")
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ing.Serve(gctx) })
	g.Go(func() error { return panel.Run(gctx, cfg.HTTPAddr) })
	g.Go(func() error { return evaluator.Run(gctx) })
	g.Go(func() error {
		// Full backfill once, then refresh the recent hours every 5 minutes
		// so long-range charts read from rollups instead of raw records.
		if err := st.UpdateRollups(gctx, time.Time{}); err != nil {
			log.Warn("rollup backfill failed", "error", err)
		} else {
			log.Info("rollup backfill complete")
		}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				if err := st.UpdateRollups(gctx, time.Now().Add(-3*time.Hour)); err != nil {
					log.Error("rollup update failed", "error", err)
				}
			}
		}
	})
	g.Go(func() error {
		if cfg.RetentionDays <= 0 {
			return nil
		}
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				n, err := st.Prune(gctx, time.Duration(cfg.RetentionDays)*24*time.Hour)
				if err != nil {
					log.Error("prune failed", "error", err)
				} else if n > 0 {
					log.Info("pruned old records", "deleted", n)
				}
				if cfg.RollupDays > 0 {
					if _, err := st.PruneRollups(gctx, time.Duration(cfg.RollupDays)*24*time.Hour); err != nil {
						log.Error("rollup prune failed", "error", err)
					}
				}
			}
		}
	})

	if err := g.Wait(); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
