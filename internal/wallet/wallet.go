// Package wallet holds the mocked coin balance. The balance is mocked, the
// lifecycle is not: every transition is an append-only ledger entry plus a locked
// account update, so the ledger always explains the balance.
package wallet

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/httpapi"
)

// auditLog is package-scoped because Reserve/Release/Consume/Restore/Credit are
// free functions shared across the redemption and webhook packages, not methods on
// a struct that could hold a logger. SetLogger lets main() attach the app's real
// structured logger; the default keeps `go test` and standalone tooling usable.
var auditLog = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "wallet_audit")

func SetLogger(l *slog.Logger) { auditLog = l.With("component", "wallet_audit") }

const (
	EntryCredit  = "CREDIT"
	EntryReserve = "RESERVE"
	EntryRelease = "RELEASE"
	EntryConsume = "CONSUME"
	EntryRestore = "RESTORE"
)

var ErrInsufficientCoins = httpapi.NewError(
	http.StatusConflict, httpapi.CodeInsufficientCoins, "not enough coins for this reward")

type Account struct {
	UserID    uuid.UUID `json:"user_id"`
	Available int64     `json:"available_coins"`
	Reserved  int64     `json:"reserved_coins"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Get(ctx context.Context, userID uuid.UUID) (Account, error) {
	a := Account{UserID: userID}
	err := s.pool.QueryRow(ctx,
		`SELECT available_coins, reserved_coins FROM wallet_accounts WHERE user_id = $1`, userID).
		Scan(&a.Available, &a.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil
	}
	return a, err
}

// lockAccount serialises concurrent claims for the same user, creating the mocked
// account on first touch.
func lockAccount(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (Account, error) {
	a := Account{UserID: userID}
	err := tx.QueryRow(ctx,
		`SELECT available_coins, reserved_coins FROM wallet_accounts WHERE user_id = $1 FOR UPDATE`,
		userID).Scan(&a.Available, &a.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil
	}
	return a, err
}

// Reserve moves coins from available to reserved. Coins are never consumed at claim.
func Reserve(ctx context.Context, tx pgx.Tx, userID, redemptionID uuid.UUID, amount int64) error {
	acct, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return err
	}
	if acct.Available < amount {
		return ErrInsufficientCoins
	}
	return apply(ctx, tx, userID, redemptionID, EntryReserve, amount, -amount, amount)
}

// Release returns reserved coins to available, for expiry, provider error or
// verified payment failure.
func Release(ctx context.Context, tx pgx.Tx, userID, redemptionID uuid.UUID, amount int64) error {
	acct, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return err
	}
	if acct.Reserved < amount {
		return errors.New("wallet: release exceeds reserved balance")
	}
	return apply(ctx, tx, userID, redemptionID, EntryRelease, amount, amount, -amount)
}

// Consume burns reserved coins once a purchase is verified against the provider.
func Consume(ctx context.Context, tx pgx.Tx, userID, redemptionID uuid.UUID, amount int64) error {
	acct, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return err
	}
	if acct.Reserved < amount {
		return errors.New("wallet: consume exceeds reserved balance")
	}
	return apply(ctx, tx, userID, redemptionID, EntryConsume, amount, 0, -amount)
}

// Restore gives spent coins back after a verified refund. The original CONSUME row
// is never modified or deleted.
func Restore(ctx context.Context, tx pgx.Tx, userID, redemptionID uuid.UUID, amount int64) error {
	if _, err := lockAccount(ctx, tx, userID); err != nil {
		return err
	}
	return apply(ctx, tx, userID, redemptionID, EntryRestore, amount, amount, 0)
}

// Credit tops up the mocked balance.
func Credit(ctx context.Context, tx pgx.Tx, userID uuid.UUID, amount int64) error {
	if _, err := lockAccount(ctx, tx, userID); err != nil {
		return err
	}
	return apply(ctx, tx, userID, uuid.Nil, EntryCredit, amount, amount, 0)
}

// apply writes the balance delta and the matching ledger row in one statement pair.
// The CHECK constraints on wallet_accounts enforce the non-negative invariants.
func apply(ctx context.Context, tx pgx.Tx, userID, redemptionID uuid.UUID, entry string, amount, availableDelta, reservedDelta int64) error {
	if amount <= 0 {
		return errors.New("wallet: amount must be positive")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_accounts (user_id, available_coins, reserved_coins)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET
			available_coins = wallet_accounts.available_coins + $2,
			reserved_coins  = wallet_accounts.reserved_coins  + $3,
			updated_at = now()`,
		userID, availableDelta, reservedDelta); err != nil {
		return err
	}

	var redemption *uuid.UUID
	if redemptionID != uuid.Nil {
		redemption = &redemptionID
	}
	ledgerID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_ledger (id, user_id, redemption_id, type, amount)
		VALUES ($1,$2,$3,$4,$5)`, ledgerID, userID, redemption, entry, amount); err != nil {
		return err
	}

	// Every coin movement is audit-logged at the moment it is durably written, so
	// the log stream and the ledger table can be cross-checked entry for entry.
	auditLog.InfoContext(ctx, "wallet ledger entry",
		"audit", true, "ledger_id", ledgerID, "user_id", userID, "redemption_id", redemptionID,
		"type", entry, "amount", amount, "available_delta", availableDelta, "reserved_delta", reservedDelta)
	return nil
}

type LedgerEntry struct {
	Type         string     `json:"type"`
	Amount       int64      `json:"amount"`
	RedemptionID *uuid.UUID `json:"redemption_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (s *Store) Ledger(ctx context.Context, userID uuid.UUID, limit int) ([]LedgerEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT type, amount, redemption_id, created_at
		FROM wallet_ledger WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.Type, &e.Amount, &e.RedemptionID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
