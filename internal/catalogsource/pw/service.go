package pw

import (
	"context"
	"log/slog"
	"time"

	"course-coupon/internal/config"
	"github.com/google/uuid"
)

type Source interface {
	Discover(context.Context, uuid.UUID) ([]Target, error)
	Acquire(context.Context, uuid.UUID, Target) (PWBatchDTO, error)
}

type BatchScrapeResult struct {
	ObservationID uuid.UUID        `json:"observation_id"`
	Target        Target           `json:"target"`
	Batch         *NormalizedBatch `json:"batch,omitempty"`
	Warnings      []string         `json:"warnings,omitempty"`
	ErrorCode     string           `json:"error_code,omitempty"`
	Error         string           `json:"error,omitempty"`
}

// RawObservation is a local diagnostic record linked to one normalized item.
// It contains only matched public batch objects and selected rendered fields.
type RawObservation struct {
	ObservationID uuid.UUID  `json:"observation_id"`
	Slug          string     `json:"slug"`
	SourceURL     string     `json:"source_url,omitempty"`
	AcquiredAt    *time.Time `json:"acquired_at,omitempty"`
	Capture       RawCapture `json:"capture"`
	Warnings      []string   `json:"warnings,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
}

type BulkScrapeResult struct {
	RunID            uuid.UUID           `json:"run_id"`
	ListingURL       string              `json:"listing_url"`
	ExtractorVersion string              `json:"extractor_version"`
	ErrorCode        string              `json:"error_code,omitempty"`
	Error            string              `json:"error,omitempty"`
	StartedAt        time.Time           `json:"started_at"`
	FinishedAt       time.Time           `json:"finished_at"`
	Discovered       int                 `json:"discovered"`
	Succeeded        int                 `json:"succeeded"`
	Failed           int                 `json:"failed"`
	WithThumbnail    int                 `json:"with_thumbnail"`
	WithPrice        int                 `json:"with_price"`
	WithPlans        int                 `json:"with_plans"`
	WarningCount     int                 `json:"warning_count"`
	RawReportFile    string              `json:"raw_report_file,omitempty"`
	Items            []BatchScrapeResult `json:"items"`
	RawObservations  []RawObservation    `json:"-"`
}

type Service struct {
	Source Source
	Config config.PWConfig
	Log    *slog.Logger
}

// Scrape discovers targets once and processes them sequentially. Per-target
// failures are returned in the report and do not stop later targets.
func (s *Service) Scrape(ctx context.Context) (BulkScrapeResult, error) {
	runID := uuid.New()
	result := BulkScrapeResult{
		RunID: runID, ListingURL: s.Config.ListingURL,
		ExtractorVersion: s.Config.ExtractorVersion,
		StartedAt:        time.Now().UTC(), Items: []BatchScrapeResult{},
	}
	targets, err := s.Source.Discover(ctx, runID)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.ErrorCode = errorCode(err)
		result.Error = err.Error()
		s.Log.ErrorContext(ctx, "PW discovery failed", "run_id", runID,
			"listing_url", result.ListingURL, "error_code", result.ErrorCode,
			"error", err, "duration_ms", result.FinishedAt.Sub(result.StartedAt).Milliseconds())
		return result, err
	}
	result.Discovered = len(targets)
	discoveredSlugs := make(map[string]struct{}, len(targets))
	for index, target := range targets {
		discoveredSlugs[target.Slug] = struct{}{}
		if err := ctx.Err(); err != nil {
			result.FinishedAt = time.Now().UTC()
			return result, failure("cancelled", err)
		}
		item := BatchScrapeResult{ObservationID: uuid.New(), Target: target}
		itemStarted := time.Now()
		s.Log.InfoContext(ctx, "PW batch scrape started", "run_id", runID, "observation_id", item.ObservationID,
			"batch_index", index+1, "batch_total", len(targets), "slug", target.Slug,
			"source_url", target.CanonicalURL)
		dto, acquireErr := s.Source.Acquire(ctx, runID, target)
		observation := RawObservation{
			ObservationID: item.ObservationID, Slug: target.Slug,
			SourceURL: target.CanonicalURL,
			Capture:   dto.RawCapture, Warnings: dto.Warnings,
		}
		if !dto.AcquiredAt.IsZero() {
			observation.AcquiredAt = &dto.AcquiredAt
		}
		if acquireErr != nil {
			item.ErrorCode = errorCode(acquireErr)
			item.Error = acquireErr.Error()
			item.Warnings = dto.Warnings
			observation.ErrorCode = item.ErrorCode
			result.RawObservations = append(result.RawObservations, observation)
			result.Failed++
			result.Items = append(result.Items, item)
			s.Log.ErrorContext(ctx, "PW batch scrape failed", "run_id", runID, "observation_id", item.ObservationID,
				"batch_index", index+1, "batch_total", len(targets), "slug", target.Slug,
				"error_code", item.ErrorCode, "duration_ms", time.Since(itemStarted).Milliseconds())
			continue
		}
		batch, warnings, normalizeErr := Normalize(dto)
		item.Warnings = warnings
		observation.Warnings = warnings
		result.WarningCount += len(warnings)
		if normalizeErr != nil {
			item.ErrorCode = errorCode(normalizeErr)
			item.Error = normalizeErr.Error()
			observation.ErrorCode = item.ErrorCode
			result.Failed++
		} else {
			item.Batch = &batch
			result.Succeeded++
			if batch.Thumbnail != "" {
				result.WithThumbnail++
			}
			if batch.SellingPriceMinor != nil {
				result.WithPrice++
			}
			if len(batch.Plans) > 0 {
				result.WithPlans++
			}
		}
		result.RawObservations = append(result.RawObservations, observation)
		result.Items = append(result.Items, item)
		s.Log.InfoContext(ctx, "PW batch scrape completed", "run_id", runID, "observation_id", item.ObservationID,
			"batch_index", index+1, "batch_total", len(targets), "slug", target.Slug,
			"result", map[bool]string{true: "failed", false: "success"}[normalizeErr != nil],
			"thumbnail_extracted", batch.Thumbnail != "", "plans_extracted", len(batch.Plans),
			"warnings", len(warnings), "duration_ms", time.Since(itemStarted).Milliseconds())
	}
	for slug := range s.Config.BatchSlugs {
		if _, found := discoveredSlugs[slug]; found {
			continue
		}
		observationID := uuid.New()
		result.Items = append(result.Items, BatchScrapeResult{
			ObservationID: observationID, Target: Target{Slug: slug}, ErrorCode: "target_not_discovered",
			Error: "configured slug was not present on the public listing",
		})
		result.RawObservations = append(result.RawObservations, RawObservation{
			ObservationID: observationID, Slug: slug,
			ErrorCode: "target_not_discovered",
		})
		result.Failed++
	}
	result.FinishedAt = time.Now().UTC()
	s.Log.InfoContext(ctx, "PW bulk scrape finished",
		"run_id", runID, "listing_url", result.ListingURL,
		"discovered", result.Discovered, "succeeded", result.Succeeded,
		"failed", result.Failed, "with_thumbnail", result.WithThumbnail,
		"with_price", result.WithPrice, "with_plans", result.WithPlans,
		"warning_count", result.WarningCount,
		"duration_ms", result.FinishedAt.Sub(result.StartedAt).Milliseconds())
	return result, nil
}
