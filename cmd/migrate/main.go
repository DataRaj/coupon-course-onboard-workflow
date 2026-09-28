// Command migrate applies pending SQL migrations against DATABASE_URL and exits.
// The api and worker binaries also do this on startup; this exists for CI/deploy
// steps that want migration to happen as its own auditable step.
package main

import (
	"context"
	"log/slog"
	"os"

	"course-coupon/internal/config"
	"course-coupon/internal/database"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
	log.Info("migrations applied")
}
