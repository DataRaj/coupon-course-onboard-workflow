package redemption

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/database"
	"course-coupon/internal/money"
)

// auditLog records every redemption state transition. Package-scoped for the same
// reason as wallet.auditLog: SetStatus/ConfirmPurchase/MarkRefunded are free
// functions called from both this package and the webhook processor.
var auditLog = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "redemption_audit")

func SetLogger(l *slog.Logger) { auditLog = l.With("component", "redemption_audit") }

var (
	ErrNotFound = errors.New("redemption not found")
	// ErrActiveExists is raised by the partial unique index guarding one in-flight
	// redemption per user+course+offer.
	ErrActiveExists = errors.New("an active redemption already exists")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var columnNames = []string{
	"id", "user_id", "course_id", "offer_id", "provider", "provider_product_id",
	"provider_plan_id", "price_snapshot_id", "mrp_amount", "provider_price_amount",
	"discount_bps", "coin_cost", "expected_discount_amount", "expected_checkout_amount",
	"currency", "status", "coupon_id", "provider_customer_id", "provider_invoice_id",
	"provider_transaction_id", "quote_verified_at", "expires_at", "checkout_opened_at",
	"purchase_confirmed_at", "refunded_at", "created_at", "updated_at",
}

// columns selects the redemption row; qualified() does the same inside a join.
var columns = strings.Join(columnNames, ", ")

func qualified(alias string) string {
	return alias + "." + strings.Join(columnNames, ", "+alias+".")
}

func scan(row pgx.Row) (Redemption, error) {
	var r Redemption
	var mrp *int64
	var price, discount, checkout int64
	err := row.Scan(&r.ID, &r.UserID, &r.CourseID, &r.OfferID, &r.Provider, &r.ProviderProductID,
		&r.ProviderPlanID, &r.PriceSnapshotID, &mrp, &price, &r.DiscountBps, &r.CoinCost,
		&discount, &checkout, &r.Currency, &r.Status, &r.CouponID, &r.ProviderCustomerID,
		&r.ProviderInvoiceID, &r.ProviderTransactionID, &r.QuoteVerifiedAt, &r.ExpiresAt,
		&r.CheckoutOpenedAt, &r.PurchaseConfirmedAt, &r.RefundedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	r.ProviderPrice = money.Minor(price)
	r.ExpectedDiscountAmount = money.Minor(discount)
	r.ExpectedCheckoutAmount = money.Minor(checkout)
	if mrp != nil {
		m := money.Minor(*mrp)
		r.MRP = &m
	}
	return r, nil
}

func (s *Store) Insert(ctx context.Context, tx pgx.Tx, r Redemption) error {
	var mrp *int64
	if r.MRP != nil {
		v := int64(*r.MRP)
		mrp = &v
	}
	_, err := tx.Exec(ctx, `INSERT INTO redemptions (id, user_id, course_id, offer_id, provider,
		provider_product_id, provider_plan_id, price_snapshot_id, mrp_amount, provider_price_amount,
		discount_bps, coin_cost, expected_discount_amount, expected_checkout_amount, currency,
		status, quote_verified_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		r.ID, r.UserID, r.CourseID, r.OfferID, r.Provider, r.ProviderProductID, r.ProviderPlanID,
		r.PriceSnapshotID, mrp, int64(r.ProviderPrice), r.DiscountBps, r.CoinCost,
		int64(r.ExpectedDiscountAmount), int64(r.ExpectedCheckoutAmount), r.Currency,
		r.Status, r.QuoteVerifiedAt, r.ExpiresAt)
	if isUniqueViolation(err, "redemptions_one_active") {
		return ErrActiveExists
	}
	return err
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Redemption, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM redemptions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Lock selects a redemption for update; every money transition goes through it.
func Lock(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Redemption, error) {
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM redemptions WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func SetStatus(ctx context.Context, q database.Execer, id uuid.UUID, status string) error {
	_, err := q.Exec(ctx, `UPDATE redemptions SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err == nil {
		auditLog.InfoContext(ctx, "redemption status changed",
			"audit", true, "redemption_id", id, "status", status)
	}
	return err
}

func AttachCoupon(ctx context.Context, q database.Execer, id, couponID uuid.UUID, status string) error {
	_, err := q.Exec(ctx, `UPDATE redemptions SET coupon_id = $2, status = $3, updated_at = now()
		WHERE id = $1`, id, couponID, status)
	if err == nil {
		auditLog.InfoContext(ctx, "redemption status changed",
			"audit", true, "redemption_id", id, "status", status, "coupon_id", couponID)
	}
	return err
}

// UpdateQuote refreshes the frozen quote after a live provider revalidation.
func UpdateQuote(ctx context.Context, q database.Execer, id uuid.UUID, snapshotID *uuid.UUID, mrp *money.Minor, price, discount, checkout money.Minor, verifiedAt time.Time) error {
	var m *int64
	if mrp != nil {
		v := int64(*mrp)
		m = &v
	}
	_, err := q.Exec(ctx, `UPDATE redemptions SET price_snapshot_id = COALESCE($2, price_snapshot_id),
		mrp_amount = $3, provider_price_amount = $4, expected_discount_amount = $5,
		expected_checkout_amount = $6, quote_verified_at = $7, updated_at = now()
		WHERE id = $1`, id, snapshotID, m, int64(price), int64(discount), int64(checkout), verifiedAt)
	return err
}

// MarkCheckoutOpened is idempotent: opening checkout is not a purchase.
func MarkCheckoutOpened(ctx context.Context, q database.Execer, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE redemptions
		SET status = $2, checkout_opened_at = COALESCE(checkout_opened_at, now()), updated_at = now()
		WHERE id = $1 AND status IN ($3, $4)`,
		id, StatusCheckoutOpened, StatusCouponIssued, StatusCheckoutOpened)
	return err
}

// ConfirmPurchase writes the verified provider references. The unique index on
// (provider, provider_transaction_id) stops one payment confirming two redemptions.
func ConfirmPurchase(ctx context.Context, q database.Execer, id uuid.UUID, customerID, invoiceID, transactionID string) error {
	_, err := q.Exec(ctx, `UPDATE redemptions SET status = $2, provider_customer_id = $3,
		provider_invoice_id = $4, provider_transaction_id = $5,
		purchase_confirmed_at = now(), updated_at = now() WHERE id = $1`,
		id, StatusPurchaseConfirmed, nilIfEmpty(customerID), nilIfEmpty(invoiceID), nilIfEmpty(transactionID))
	if err == nil {
		auditLog.InfoContext(ctx, "redemption purchase confirmed",
			"audit", true, "redemption_id", id, "status", StatusPurchaseConfirmed,
			"provider_customer_id", customerID, "provider_invoice_id", invoiceID,
			"provider_transaction_id", transactionID)
	}
	return err
}

func MarkRefunded(ctx context.Context, q database.Execer, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE redemptions SET status = $2, refunded_at = now(), updated_at = now()
		WHERE id = $1`, id, StatusRefunded)
	if err == nil {
		auditLog.InfoContext(ctx, "redemption refunded",
			"audit", true, "redemption_id", id, "status", StatusRefunded)
	}
	return err
}

// FindByCouponCode attributes a provider payment to a redemption via the unique code.
func (s *Store) FindByCouponCode(ctx context.Context, providerCode, couponCode string) (Redemption, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+qualified("r")+` FROM redemptions r
		JOIN coupons c ON c.redemption_id = r.id
		WHERE r.provider = $1 AND c.coupon_code = $2`, providerCode, couponCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// FindByProviderRefs attributes a payment when no coupon code was exposed.
func (s *Store) FindByProviderRefs(ctx context.Context, providerCode, productID, planID, customerID, invoiceID string) (Redemption, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM redemptions
		WHERE provider = $1 AND provider_product_id = $2 AND provider_plan_id = $3
		  AND (provider_invoice_id = $4 OR provider_customer_id = $5 OR status = ANY($6))
		ORDER BY (provider_invoice_id = $4) DESC, created_at DESC
		LIMIT 1`, providerCode, productID, planID, invoiceID, customerID, liveStatuses))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ExpiredHolding claims redemptions past their TTL that still hold coins.
func ExpiredHolding(ctx context.Context, tx pgx.Tx, limit int) ([]Redemption, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM redemptions
		WHERE status = ANY($1) AND expires_at <= now()
		ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, liveStatuses, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// History returns the student's purchases with their original frozen quotes.
func (s *Store) History(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Redemption, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM redemptions
		WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Redemption, error) {
	var out []Redemption
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// IsUniqueViolation reports a duplicate-key error on any constraint.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
