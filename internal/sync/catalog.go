// Package sync keeps the local course projection eventually consistent with the
// provider catalog. Live claim/checkout revalidation remains the financial authority.
package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"course-coupon/internal/provider"
)

// catalogLockKey serialises the catalog sync across worker instances.
const catalogLockKey int64 = 0x0C0FFEE1

type Catalog struct {
	pool  *pgxpool.Pool
	store *course.Store
	prov  provider.Provider
	log   *slog.Logger
}

func NewCatalog(pool *pgxpool.Pool, store *course.Store, prov provider.Provider, log *slog.Logger) *Catalog {
	return &Catalog{pool: pool, store: store, prov: prov, log: log.With("component", "catalog_sync")}
}

// Run performs one full catalog reconciliation under an advisory lock.
func (c *Catalog) Run(ctx context.Context) error {
	locked, release, err := database.TryAdvisoryLock(ctx, c.pool, catalogLockKey)
	if err != nil {
		return err
	}
	if !locked {
		c.log.DebugContext(ctx, "catalog sync already running elsewhere")
		return nil
	}
	defer release()

	runID := uuid.New()
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO provider_sync_runs (id, provider, status) VALUES ($1,$2,'RUNNING')`,
		runID, c.prov.Code()); err != nil {
		return err
	}

	seen, changed, err := c.sync(ctx)
	if err != nil {
		// A partial scan must not deactivate anything; we simply record the failure.
		c.log.ErrorContext(ctx, "catalog sync failed", "error", err)
		_, _ = c.pool.Exec(ctx, `UPDATE provider_sync_runs
			SET status='FAILED', error=$2, finished_at=now() WHERE id=$1`, runID, err.Error())
		return err
	}

	_, err = c.pool.Exec(ctx, `UPDATE provider_sync_runs
		SET status='SUCCEEDED', items_seen=$2, items_changed=$3, finished_at=now() WHERE id=$1`,
		runID, len(seen), changed)
	c.log.InfoContext(ctx, "catalog sync complete", "items", len(seen), "price_changes", changed)
	return err
}

func (c *Catalog) sync(ctx context.Context) (seen []string, changed int, err error) {
	items, err := c.prov.ListCatalog(ctx)
	if err != nil {
		return nil, 0, err
	}

	seen = make([]string, 0, len(items))
	err = database.InTx(ctx, c.pool, func(tx pgx.Tx) error {
		for _, item := range items {
			_, priceChanged, err := c.store.Upsert(ctx, tx, c.prov.Code(), item, course.SourceSync)
			if err != nil {
				return err
			}
			seen = append(seen, item.Ref.PlanID)
			if priceChanged {
				changed++
			}
		}
		// Reached only after every page was read successfully.
		n, err := c.store.DeactivateMissing(ctx, tx, c.prov.Code(), seen)
		if err != nil {
			return err
		}
		if n > 0 {
			c.log.InfoContext(ctx, "deactivated courses no longer listed by provider", "count", n)
		}
		return nil
	})
	return seen, changed, err
}

// Loop runs the sync on an interval until the context is cancelled.
func (c *Catalog) Loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		c.log.WarnContext(ctx, "initial catalog sync failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Run(ctx); err != nil && ctx.Err() == nil {
				c.log.WarnContext(ctx, "catalog sync failed", "error", err)
			}
		}
	}
}
