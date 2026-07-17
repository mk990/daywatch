package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mk/daywatch/internal/config"
	"github.com/mk/daywatch/internal/ingest"
	"github.com/mk/daywatch/internal/store"
	"github.com/mk/daywatch/internal/web"
)

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

	ing := ingest.New(cfg.IngestAddr, cfg.TokenHash, cfg.ReadTimeout, st, log)
	if err := ing.Listen(); err != nil {
		log.Error("ingest listen failed", "error", err)
		os.Exit(1)
	}

	panel, err := web.New(st, log)
	if err != nil {
		log.Error("web init failed", "error", err)
		os.Exit(1)
	}

	if cfg.TokenHash != "" {
		log.Info("token validation enabled", "token_hash", cfg.TokenHash)
	} else {
		log.Warn("NIGHTWATCH_TOKEN not set: accepting any token")
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ing.Serve(gctx) })
	g.Go(func() error { return panel.Run(gctx, cfg.HTTPAddr) })
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
			}
		}
	})

	if err := g.Wait(); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
