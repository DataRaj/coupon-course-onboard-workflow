// Command seed prepares a local prototype run: it credits the mocked development
// wallet and attaches one active offer to every synced course that lacks one.
// Offers are platform data, so they are seeded after the provider catalog syncs.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"course-coupon/internal/config"
	"course-coupon/internal/database"
	"course-coupon/internal/wallet"
)

func main() {
	discountBps := flag.Int("discount-bps", 2000, "reward percentage in basis points")
	coinCost := flag.Int64("coin-cost", 2000, "coins charged per redemption")
	coins := flag.Int64("coins", 10000, "coins to credit the development user")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, int32(*discountBps), *coinCost, *coins); err != nil {
		log.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, discountBps int32, coinCost, coins int64) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	devUser, err := uuid.Parse(cfg.DevUserID)
	if err != nil {
		return err
	}

	return database.InTx(ctx, pool, func(tx pgx.Tx) error {
		var available int64
		if err := tx.QueryRow(ctx,
			`SELECT coalesce((SELECT available_coins FROM wallet_accounts WHERE user_id = $1), 0)`,
			devUser).Scan(&available); err != nil {
			return err
		}
		if available < coins {
			if err := wallet.Credit(ctx, tx, devUser, coins-available); err != nil {
				return err
			}
			log.Info("credited development wallet", "user_id", devUser, "coins", coins)
		}

		tag, err := tx.Exec(ctx, `INSERT INTO offers (id, course_id, discount_bps, coin_cost)
			SELECT gen_random_uuid(), c.id, $1, $2 FROM courses c
			WHERE c.provider_active
			  AND NOT EXISTS (SELECT 1 FROM offers o WHERE o.course_id = c.id AND o.status = 'ACTIVE')`,
			discountBps, coinCost)
		if err != nil {
			return err
		}
		log.Info("offers seeded", "created", tag.RowsAffected(), "discount_bps", discountBps, "coin_cost", coinCost)
		return nil
	})
}
