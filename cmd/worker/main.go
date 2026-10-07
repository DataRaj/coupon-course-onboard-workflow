package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"course-coupon/internal/catalogsource"
	"course-coupon/internal/catalogsource/pw"
	"course-coupon/internal/config"
	"course-coupon/internal/coupon"
	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"course-coupon/internal/provider/pabbly"
	"course-coupon/internal/redemption"
	catalogsync "course-coupon/internal/sync"
	"course-coupon/internal/wallet"
	"course-coupon/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "ingest-pw-batch" {
			return fmt.Errorf("usage: worker [ingest-pw-batch]")
		}
		return runPWBatch(log)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}

	// The worker is where most money-moving transitions actually happen (webhook
	// confirmation, expiry, refund), so its audit trail matters as much as the API's.
	wallet.SetLogger(log)
	redemption.SetLogger(log)
	coupon.SetLogger(log)

	pabblyClient := pabbly.New(cfg.Pabbly, log)
	courseStore := course.NewStore(pool)
	couponStore := coupon.NewStore(pool)
	redemptionStore := redemption.NewStore(pool)

	catalog := catalogsync.NewCatalog(pool, courseStore, pabblyClient, log)
	jobs := catalogsync.NewJobs(pool, couponStore, redemptionStore, pabblyClient, log)
	processor := webhook.NewProcessor(pool, redemptionStore, couponStore, pabblyClient, log)

	// Independent loops inside one worker process; PostgreSQL is the only queue.
	loops := []func(context.Context){
		func(c context.Context) { catalog.Loop(c, cfg.CatalogSyncInterval) },
		func(c context.Context) {
			catalogsync.Loop(c, "webhook_processing", 5*time.Second, log,
				func(c context.Context) (int, error) { return processor.ProcessBatch(c, 20) })
		},
		func(c context.Context) {
			catalogsync.Loop(c, "redemption_expiry", 30*time.Second, log, jobs.ExpireRedemptions)
		},
		func(c context.Context) {
			catalogsync.Loop(c, "coupon_deactivation", time.Minute, log, jobs.DeactivateCoupons)
		},
	}

	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}

	log.Info("worker started", "catalog_sync_interval", cfg.CatalogSyncInterval.String())
	<-ctx.Done()
	wg.Wait()
	log.Info("worker stopped")
	return nil
}

// runPWBatch is manual worker-side work. Ordinary API requests never start a browser.
func runPWBatch(log *slog.Logger) error {
	pwcfg, databaseURL, err := config.LoadPWCommand()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := database.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	browser := pw.NewBrowser(pwcfg, log)
	defer browser.Close()
	service := pw.Service{Store: catalogsource.NewStore(pool), Source: browser, Config: pwcfg, Log: log}
	id, err := service.IngestTargetBatch(ctx)
	if err != nil {
		return err
	}
	c, err := course.NewStore(pool).Get(ctx, id)
	if err != nil {
		return err
	}
	log.Info("PW normalized course available", "course_id", id, "api_path", "/api/v1/courses/"+id.String(), "course", course.ToView(c, nil))
	return nil
}
