package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"course-coupon/internal/admin"
	"course-coupon/internal/auth"
	"course-coupon/internal/config"
	"course-coupon/internal/coupon"
	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"course-coupon/internal/httpapi"
	"course-coupon/internal/idempotency"
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
		log.Error("api exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
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

	authenticator, err := auth.New(cfg, log)
	if err != nil {
		return err
	}

	// Route the audit trail (coin movements, redemption/coupon status changes)
	// through the same structured JSON logger and output stream as everything else.
	wallet.SetLogger(log)
	redemption.SetLogger(log)
	coupon.SetLogger(log)

	pabblyClient := pabbly.New(cfg.Pabbly, log)
	courseStore := course.NewStore(pool)
	couponStore := coupon.NewStore(pool)
	redemptionStore := redemption.NewStore(pool)
	walletStore := wallet.NewStore(pool)
	keys := idempotency.NewStore(pool)

	redemptionSvc := redemption.NewService(pool, courseStore, couponStore, redemptionStore,
		pabblyClient, cfg, log)
	catalog := catalogsync.NewCatalog(pool, courseStore, pabblyClient, log)
	processor := webhook.NewProcessor(pool, redemptionStore, couponStore, pabblyClient, log)

	router := httpapi.NewRouter(httpapi.RouterOptions{
		Log:  log,
		Auth: authenticator.Middleware,
		// The provider posts webhooks without our application token; the unguessable
		// path segment is the shared secret.
		Public: []httpapi.Routable{webhook.NewIngest(pool, cfg.WebhookSecret, log)},
		Protected: []httpapi.Routable{
			course.NewHandler(courseStore, log),
			wallet.NewHandler(walletStore, log),
			redemption.NewHandler(redemptionSvc, keys, log),
			admin.NewHandler(catalog, processor, redemptionSvc, log),
		},
		HealthFunc: func() error { return pool.Ping(ctx) },
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
