package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
	var logOutput io.Writer = os.Stdout
	var logFile *os.File
	if len(os.Args) == 2 && os.Args[1] == "scrape-pw-batches" {
		// Keep stdout as one machine-readable bulk report.
		logOutput = os.Stderr
		if path := strings.TrimSpace(os.Getenv("PW_LOG_FILE")); path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "create PW log directory: %v\n", err)
				os.Exit(1)
			}
			var err error
			logFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				fmt.Fprintf(os.Stderr, "open PW log file: %v\n", err)
				os.Exit(1)
			}
			defer logFile.Close()
			logOutput = io.MultiWriter(os.Stderr, logFile)
		}
	}
	log := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "scrape-pw-batches" {
			return fmt.Errorf("usage: worker [scrape-pw-batches]")
		}
		return runPWBatches(log)
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

// runPWBatches is a manual worker-side scrape. It does not open the database or
// modify marketplace state, and ordinary API requests never start a browser.
func runPWBatches(log *slog.Logger) error {
	pwcfg, err := config.LoadPW()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	browser := pw.NewBrowser(pwcfg, log)
	service := pw.Service{Source: browser, Config: pwcfg, Log: log}
	result, scrapeErr := service.Scrape(ctx)
	closeErr := browser.Close()
	var rawSaveErr error
	rawPath := strings.TrimSpace(os.Getenv("PW_RAW_OUTPUT_FILE"))
	if rawPath == "" {
		if outputPath := strings.TrimSpace(os.Getenv("PW_OUTPUT_FILE")); outputPath != "" {
			rawPath = strings.TrimSuffix(outputPath, ".json") + "-raw.json"
		}
	}
	if rawPath != "" {
		rawPath = strings.TrimSuffix(rawPath, ".json") + "-" + result.RunID.String() + ".json"
		rawReport := struct {
			RunID            string              `json:"run_id"`
			ExtractorVersion string              `json:"extractor_version"`
			Observations     []pw.RawObservation `json:"observations"`
		}{result.RunID.String(), result.ExtractorVersion, result.RawObservations}
		var rawJSON []byte
		rawJSON, rawSaveErr = json.MarshalIndent(rawReport, "", "  ")
		if rawSaveErr == nil {
			rawJSON = append(rawJSON, '\n')
			if mkdirErr := os.MkdirAll(filepath.Dir(rawPath), 0o755); mkdirErr != nil {
				rawSaveErr = fmt.Errorf("create PW raw output directory: %w", mkdirErr)
			} else if writeErr := os.WriteFile(rawPath, rawJSON, 0o600); writeErr != nil {
				rawSaveErr = fmt.Errorf("save PW raw observations: %w", writeErr)
			} else {
				result.RawReportFile = rawPath
				log.Info("PW raw observations saved", "output_file", rawPath, "observations", len(result.RawObservations))
			}
		}
	}
	report, encodeErr := json.MarshalIndent(result, "", "  ")
	if encodeErr == nil {
		report = append(report, '\n')
		_, encodeErr = os.Stdout.Write(report)
	}
	var saveErr error
	if path := strings.TrimSpace(os.Getenv("PW_OUTPUT_FILE")); path != "" && len(report) > 0 {
		if scrapeErr != nil {
			path = strings.TrimSuffix(path, ".json") + "-failed.json"
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o755); mkdirErr != nil {
			saveErr = fmt.Errorf("create PW output directory: %w", mkdirErr)
		} else if writeErr := os.WriteFile(path, report, 0o600); writeErr != nil {
			saveErr = fmt.Errorf("save PW output report: %w", writeErr)
		} else {
			log.Info("PW JSON report saved", "output_file", path, "items", len(result.Items))
		}
	}
	return errors.Join(scrapeErr, closeErr, rawSaveErr, encodeErr, saveErr)
}
