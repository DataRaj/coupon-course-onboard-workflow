package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/coupon"
	"course-coupon/internal/database"
	"course-coupon/internal/provider"
	"course-coupon/internal/redemption"
	"course-coupon/internal/wallet"
)

const batchSize = 50

// Jobs bundles the small recurring maintenance loops the worker runs.
type Jobs struct {
	pool    *pgxpool.Pool
	coupons *coupon.Store
	redeem  *redemption.Store
	prov    provider.Provider
	log     *slog.Logger
}

func NewJobs(pool *pgxpool.Pool, coupons *coupon.Store, redeem *redemption.Store, prov provider.Provider, log *slog.Logger) *Jobs {
	return &Jobs{pool: pool, coupons: coupons, redeem: redeem, prov: prov,
		log: log.With("component", "jobs")}
}

// ExpireRedemptions releases coins held by redemptions past their local TTL and
// queues the provider coupon for deactivation.
func (j *Jobs) ExpireRedemptions(ctx context.Context) (int, error) {
	var expired int
	err := database.InTx(ctx, j.pool, func(tx pgx.Tx) error {
		due, err := redemption.ExpiredHolding(ctx, tx, batchSize)
		if err != nil {
			return err
		}
		for _, r := range due {
			if err := redemption.SetStatus(ctx, tx, r.ID, redemption.StatusExpired); err != nil {
				return err
			}
			if r.CouponID != nil {
				if err := j.coupons.SetStatus(ctx, tx, *r.CouponID, coupon.StatusExpired); err != nil {
					return err
				}
				if err := j.coupons.ScheduleDeactivation(ctx, tx, *r.CouponID, time.Now().UTC()); err != nil {
					return err
				}
			}
			if err := wallet.Release(ctx, tx, r.UserID, r.ID, r.CoinCost); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	if expired > 0 {
		j.log.InfoContext(ctx, "expired redemptions released", "count", expired)
	}
	return expired, err
}

// DeactivateCoupons retries provider-side deactivation. Coins are never re-reserved
// because a remote cleanup call failed; only the cleanup is retried.
func (j *Jobs) DeactivateCoupons(ctx context.Context) (int, error) {
	var pending []coupon.Coupon
	if err := database.InTx(ctx, j.pool, func(tx pgx.Tx) error {
		var err error
		pending, err = j.coupons.PendingDeactivation(ctx, tx, batchSize)
		return err
	}); err != nil {
		return 0, err
	}

	var done int
	for _, c := range pending {
		if c.ProviderCouponID == nil {
			continue
		}
		// Deactivate rather than delete so the reward history survives.
		if err := j.prov.DisableCoupon(ctx, c.ProviderProductID, *c.ProviderCouponID); err != nil {
			j.log.WarnContext(ctx, "coupon deactivation failed, will retry",
				"coupon_id", c.ID, "error", err)
			if err := j.coupons.ScheduleDeactivation(ctx, j.pool, c.ID, time.Now().UTC().Add(5*time.Minute)); err != nil {
				return done, err
			}
			continue
		}
		if err := j.coupons.ClearDeactivation(ctx, j.pool, c.ID); err != nil {
			return done, err
		}
		done++
	}
	if done > 0 {
		j.log.InfoContext(ctx, "coupons deactivated at provider", "count", done)
	}
	return done, nil
}

// Loop runs a job on an interval, logging but not propagating per-tick failures.
func Loop(ctx context.Context, name string, interval time.Duration, log *slog.Logger, fn func(context.Context) (int, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fn(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "job failed", "job", name, "error", err)
			}
		}
	}
}
