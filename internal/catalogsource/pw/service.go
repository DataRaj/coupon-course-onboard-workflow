package pw

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"course-coupon/internal/catalogsource"
	"course-coupon/internal/config"
	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"github.com/google/uuid"
)

// Acquirer allows offline failure/fixture testing without a browser.
type Acquirer interface {
	Acquire(context.Context, uuid.UUID) (PWBatchDTO, error)
}
type Service struct {
	Store  *catalogsource.Store
	Source Acquirer
	Config config.PWConfig
	Log    *slog.Logger
}

var ErrIncomplete = errors.New("PW commercial observation incomplete; last-known-good projection preserved")

func (s *Service) IngestTargetBatch(ctx context.Context) (id uuid.UUID, err error) {
	slug, canonical, err := targetIdentity(s.Config.BatchURL)
	if err != nil {
		return id, err
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(Provider + ":" + slug))
	locked, release, err := database.TryAdvisoryLock(ctx, s.Store.Pool, int64(h.Sum64()))
	if err != nil {
		return id, err
	}
	if !locked {
		return id, failure("already_running", fmt.Errorf("target ingestion already holds PostgreSQL lock"))
	}
	defer release()
	run, err := s.Store.Start(ctx, Provider, slug, canonical, s.Config.ExtractorVersion)
	if err != nil {
		return id, err
	}
	log := s.Log.With("provider", Provider, "source_url", canonical, "ingestion_run_id", run.ID, "extractor_version", s.Config.ExtractorVersion)
	started := time.Now()
	var d PWBatchDTO
	var observation *course.CatalogObservation
	var diagnostics catalogsource.Diagnostics
	var changed bool
	// Use a short independent context to record failures after caller cancellation.
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err != nil {
			diagnostics.ErrorCode = errorCode(err)
			diagnostics.Error = err.Error()
		}
		var persistErr error
		id, changed, persistErr = s.Store.Finish(finishCtx, run, observation, diagnostics)
		if persistErr != nil {
			// The failed transaction left the run RUNNING; finalize it without projection.
			fallbackID, _, recordErr := s.Store.Finish(finishCtx, run, nil, catalogsource.Diagnostics{ErrorCode: "persistence_failed", Error: "normalized projection transaction failed"})
			if recordErr == nil {
				id = fallbackID
			}
			err = errors.Join(err, persistErr, recordErr)
		}
		result := "SUCCEEDED"
		if err != nil {
			result = "FAILED"
		} else if observation != nil && observation.SellingPrice == nil {
			result = "INCOMPLETE"
			err = ErrIncomplete
		}
		log.Info("PW ingestion finished", "course_id", id, "external_id", d.ExternalID, "result", result, "error_code", diagnostics.ErrorCode, "duration_ms", time.Since(started).Milliseconds(), "changed", changed, "warnings", diagnostics.Warnings)
	}()
	d, err = s.Source.Acquire(ctx, run.ID)
	diagnostics.Provenance = d.Provenance
	diagnostics.Warnings = d.Warnings
	if !d.AcquiredAt.IsZero() {
		diagnostics.AcquiredAt = &d.AcquiredAt
	}
	if err != nil {
		return id, err
	}
	o, warnings, err := Normalize(d, s.Config.ExpectedTitle)
	diagnostics.Warnings = warnings
	if err != nil {
		return id, err
	}
	if ctx.Err() != nil {
		return id, failure("cancelled", ctx.Err())
	}
	observation = &o
	// API representation is normalized; do not log raw browser payloads.
	log.InfoContext(ctx, "PW validation complete", "commercial_complete", o.SellingPrice != nil, "title", o.Title, "class", o.Class, "currency", o.Currency, "selling_price_minor", o.SellingPrice, "fields_extracted", len(d.Provenance))
	return id, nil
}
