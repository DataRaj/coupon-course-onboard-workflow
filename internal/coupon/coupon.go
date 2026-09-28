// Package coupon owns the unique student reward code and its local lifecycle.
// The coupon is an artefact of a redemption, not the business transaction itself.
package coupon

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/database"
)

// auditLog records coupon status changes — the reward code lifecycle is the other
// half (with wallet and redemption) of the auditable money trail.
var auditLog = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "coupon_audit")

func SetLogger(l *slog.Logger) { auditLog = l.With("component", "coupon_audit") }

const (
	StatusCreating = "CREATING"
	StatusActive   = "ACTIVE"
	StatusUsed     = "USED"
	StatusDisabled = "DISABLED"
	StatusExpired  = "EXPIRED"
	StatusFailed   = "FAILED"
)

// codeAlphabet omits characters that are easy to misread when typed by hand.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const codePrefix = "STU-"

// GenerateCode returns an unguessable code. It carries no student identity: no
// email, phone, user id, school or class.
func GenerateCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := []byte(codePrefix + "________")
	for i, b := range buf {
		out[len(codePrefix)+i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

type Coupon struct {
	ID                uuid.UUID
	RedemptionID      uuid.UUID
	Provider          string
	ProviderCouponID  *string
	Code              string
	DiscountBps       int32
	Status            string
	ProviderProductID string
	ProviderPlanID    string
	ExpiresAt         time.Time
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var ErrNotFound = errors.New("coupon not found")

const columns = `id, redemption_id, provider, provider_coupon_id, coupon_code, discount_bps,
	status, provider_product_id, provider_plan_id, expires_at`

func scan(row pgx.Row) (Coupon, error) {
	var c Coupon
	err := row.Scan(&c.ID, &c.RedemptionID, &c.Provider, &c.ProviderCouponID, &c.Code,
		&c.DiscountBps, &c.Status, &c.ProviderProductID, &c.ProviderPlanID, &c.ExpiresAt)
	return c, err
}

// Create records the coupon locally before the provider call, so an ambiguous
// timeout still leaves us holding the exact code we asked for.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, c Coupon) error {
	_, err := tx.Exec(ctx, `INSERT INTO coupons (id, redemption_id, provider, coupon_code,
		discount_bps, status, provider_product_id, provider_plan_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		c.ID, c.RedemptionID, c.Provider, c.Code, c.DiscountBps, c.Status,
		c.ProviderProductID, c.ProviderPlanID, c.ExpiresAt)
	if err == nil {
		// The code is logged (it is not sensitive — no PII is embedded in it) so a
		// support agent can trace a student's coupon end to end from the log stream.
		auditLog.InfoContext(ctx, "coupon created locally",
			"audit", true, "coupon_id", c.ID, "redemption_id", c.RedemptionID,
			"code", c.Code, "status", c.Status)
	}
	return err
}

func (s *Store) ByRedemption(ctx context.Context, redemptionID uuid.UUID) (Coupon, error) {
	c, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM coupons WHERE redemption_id = $1`, redemptionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// MarkIssued attaches the provider identifier once creation is confirmed.
func (s *Store) MarkIssued(ctx context.Context, q database.Execer, id uuid.UUID, providerCouponID string) error {
	_, err := q.Exec(ctx, `UPDATE coupons SET status = $2, provider_coupon_id = $3, updated_at = now()
		WHERE id = $1`, id, StatusActive, providerCouponID)
	if err == nil {
		auditLog.InfoContext(ctx, "coupon issued at provider",
			"audit", true, "coupon_id", id, "status", StatusActive, "provider_coupon_id", providerCouponID)
	}
	return err
}

// SetStatus moves the local coupon lifecycle forward.
func (s *Store) SetStatus(ctx context.Context, q database.Execer, id uuid.UUID, status string) error {
	_, err := q.Exec(ctx, `UPDATE coupons SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err == nil {
		auditLog.InfoContext(ctx, "coupon status changed", "audit", true, "coupon_id", id, "status", status)
	}
	return err
}

// ScheduleDeactivation records that the provider-side coupon still owes a disable
// call. Coins are never re-reserved because a remote cleanup failed.
func (s *Store) ScheduleDeactivation(ctx context.Context, q database.Execer, id uuid.UUID, retryAt time.Time) error {
	_, err := q.Exec(ctx, `UPDATE coupons SET deactivate_after = $2, updated_at = now() WHERE id = $1`,
		id, retryAt)
	return err
}

func (s *Store) ClearDeactivation(ctx context.Context, q database.Execer, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE coupons SET deactivate_after = NULL, updated_at = now() WHERE id = $1`, id)
	return err
}

// PendingDeactivation claims coupons whose remote deactivation must be retried.
func (s *Store) PendingDeactivation(ctx context.Context, tx pgx.Tx, limit int) ([]Coupon, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM coupons
		WHERE deactivate_after IS NOT NULL AND deactivate_after <= now()
		  AND provider_coupon_id IS NOT NULL
		ORDER BY deactivate_after
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Coupon
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
