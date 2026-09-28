package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/coupon"
	"course-coupon/internal/database"
	"course-coupon/internal/provider"
	"course-coupon/internal/redemption"
	"course-coupon/internal/wallet"
)

const (
	StatusReceived   = "RECEIVED"
	StatusProcessing = "PROCESSING"
	StatusProcessed  = "PROCESSED"
	StatusIgnored    = "IGNORED"
	StatusFailed     = "FAILED"
)

const maxAttempts = 5

type Processor struct {
	pool    *pgxpool.Pool
	redeem  *redemption.Store
	coupons *coupon.Store
	prov    provider.Provider
	log     *slog.Logger
}

func NewProcessor(pool *pgxpool.Pool, redeem *redemption.Store, coupons *coupon.Store, prov provider.Provider, log *slog.Logger) *Processor {
	return &Processor{pool: pool, redeem: redeem, coupons: coupons, prov: prov,
		log: log.With("component", "webhook_processor")}
}

type event struct {
	ID        uuid.UUID
	EventType string
	Payload   map[string]any
	Attempts  int
}

// ProcessBatch claims pending events with SKIP LOCKED so several workers can share
// the queue, then reconciles each against the provider API.
func (p *Processor) ProcessBatch(ctx context.Context, limit int) (int, error) {
	var events []event
	err := database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, event_type, payload, attempt_count
			FROM provider_webhook_events
			WHERE status IN ($1, $2) AND attempt_count < $3
			ORDER BY received_at
			LIMIT $4 FOR UPDATE SKIP LOCKED`, StatusReceived, StatusProcessing, maxAttempts, limit)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e event
			var raw []byte
			if err := rows.Scan(&e.ID, &e.EventType, &raw, &e.Attempts); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &e.Payload); err != nil {
				return err
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, e := range events {
			if _, err := tx.Exec(ctx, `UPDATE provider_webhook_events
				SET status = $2, attempt_count = attempt_count + 1 WHERE id = $1`,
				e.ID, StatusProcessing); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	for _, e := range events {
		p.handle(ctx, e)
	}
	return len(events), nil
}

func (p *Processor) handle(ctx context.Context, e event) {
	status, err := p.dispatch(ctx, e)
	switch {
	case err != nil && e.Attempts+1 >= maxAttempts:
		p.log.ErrorContext(ctx, "webhook permanently failed", "event_id", e.ID, "error", err)
		p.finish(ctx, e.ID, StatusFailed, err)
	case err != nil:
		p.log.WarnContext(ctx, "webhook processing failed, will retry", "event_id", e.ID, "error", err)
		p.finish(ctx, e.ID, StatusReceived, err)
	default:
		p.finish(ctx, e.ID, status, nil)
	}
}

func (p *Processor) finish(ctx context.Context, id uuid.UUID, status string, cause error) {
	var msg *string
	if cause != nil {
		s := cause.Error()
		msg = &s
	}
	var processedAt *time.Time
	if status == StatusProcessed || status == StatusIgnored || status == StatusFailed {
		now := time.Now().UTC()
		processedAt = &now
	}
	if _, err := p.pool.Exec(ctx, `UPDATE provider_webhook_events
		SET status = $2, last_error = $3, processed_at = $4 WHERE id = $1`,
		id, status, msg, processedAt); err != nil {
		p.log.ErrorContext(ctx, "failed to record webhook outcome", "event_id", id, "error", err)
	}
}

func (p *Processor) dispatch(ctx context.Context, e event) (string, error) {
	switch classify(e.EventType) {
	case kindPaymentSuccess:
		return p.handlePayment(ctx, e, true)
	case kindPaymentFailure:
		return p.handlePayment(ctx, e, false)
	case kindRefund:
		return p.handleRefund(ctx, e)
	default:
		return StatusIgnored, nil
	}
}

type eventKind int

const (
	kindOther eventKind = iota
	kindPaymentSuccess
	kindPaymentFailure
	kindRefund
)

func classify(eventType string) eventKind {
	t := strings.ToLower(strings.ReplaceAll(eventType, " ", "_"))
	switch {
	case strings.Contains(t, "refund"):
		return kindRefund
	case strings.Contains(t, "fail"):
		return kindPaymentFailure
	case strings.Contains(t, "success") || strings.Contains(t, "payment_made") || t == "payment":
		return kindPaymentSuccess
	}
	return kindOther
}

// handlePayment verifies the notification against Pabbly before any coin moves.
func (p *Processor) handlePayment(ctx context.Context, e event, claimsSuccess bool) (string, error) {
	refs := extractRefs(e.Payload)

	red, err := p.attribute(ctx, refs)
	if errors.Is(err, redemption.ErrNotFound) {
		// A payment made outside the marketplace is not ours to act on.
		p.log.InfoContext(ctx, "webhook did not match any redemption", "event_id", e.ID)
		return StatusIgnored, nil
	}
	if err != nil {
		return "", err
	}

	state, err := p.prov.VerifyPurchase(ctx, provider.PurchaseRef{
		InvoiceID:     refs.invoiceID,
		CustomerID:    refs.customerID,
		TransactionID: refs.transactionID,
		Ref:           provider.CourseRef{ProductID: red.ProviderProductID, PlanID: red.ProviderPlanID},
	})
	if err != nil {
		return "", fmt.Errorf("verify purchase: %w", err)
	}

	switch {
	case state.Paid:
		if err := p.verifyMatches(red, state); err != nil {
			return "", err
		}
		return StatusProcessed, p.confirm(ctx, red, state)
	case state.Failed || !claimsSuccess:
		return StatusProcessed, p.fail(ctx, red)
	default:
		// Neither paid nor failed at the provider yet; retry until it settles.
		return "", errors.New("provider has not settled this payment yet")
	}
}

// verifyMatches refuses to confirm a payment that does not match the redemption.
func (p *Processor) verifyMatches(red redemption.Redemption, state provider.PurchaseState) error {
	if state.ProductID != "" && state.ProductID != red.ProviderProductID {
		return fmt.Errorf("provider product mismatch for redemption %s", red.ID)
	}
	if state.PlanID != "" && state.PlanID != red.ProviderPlanID {
		return fmt.Errorf("provider plan mismatch for redemption %s", red.ID)
	}
	if state.Currency != "" && state.Currency != red.Currency {
		return fmt.Errorf("currency mismatch for redemption %s", red.ID)
	}
	return nil
}

// confirm consumes the reserved coins exactly once, under a row lock.
func (p *Processor) confirm(ctx context.Context, red redemption.Redemption, state provider.PurchaseState) error {
	return database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		locked, err := redemption.Lock(ctx, tx, red.ID)
		if err != nil {
			return err
		}
		// Duplicate delivery of the same payment must not consume coins twice.
		if locked.Status == redemption.StatusPurchaseConfirmed {
			return nil
		}
		if !locked.HoldsCoins() {
			return fmt.Errorf("redemption %s is in state %s and cannot be confirmed", locked.ID, locked.Status)
		}

		if err := redemption.ConfirmPurchase(ctx, tx, locked.ID,
			state.CustomerID, state.InvoiceID, state.TransactionID); err != nil {
			if redemption.IsUniqueViolation(err) {
				return fmt.Errorf("provider transaction %s already applied elsewhere", state.TransactionID)
			}
			return err
		}
		if locked.CouponID != nil {
			if err := p.coupons.SetStatus(ctx, tx, *locked.CouponID, coupon.StatusUsed); err != nil {
				return err
			}
		}
		if err := recordTransaction(ctx, tx, locked, state, "PAYMENT"); err != nil {
			return err
		}
		return wallet.Consume(ctx, tx, locked.UserID, locked.ID, locked.CoinCost)
	})
}

// fail releases the reservation and retires the unused coupon.
func (p *Processor) fail(ctx context.Context, red redemption.Redemption) error {
	return database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		locked, err := redemption.Lock(ctx, tx, red.ID)
		if err != nil {
			return err
		}
		if !locked.HoldsCoins() {
			return nil
		}
		if err := redemption.SetStatus(ctx, tx, locked.ID, redemption.StatusPaymentFailed); err != nil {
			return err
		}
		if locked.CouponID != nil {
			if err := p.coupons.SetStatus(ctx, tx, *locked.CouponID, coupon.StatusDisabled); err != nil {
				return err
			}
			// Remote deactivation is retried by the worker; it never blocks the release.
			if err := p.coupons.ScheduleDeactivation(ctx, tx, *locked.CouponID, time.Now().UTC()); err != nil {
				return err
			}
		}
		return wallet.Release(ctx, tx, locked.UserID, locked.ID, locked.CoinCost)
	})
}

// handleRefund restores coins once, only after the provider confirms the refund.
func (p *Processor) handleRefund(ctx context.Context, e event) (string, error) {
	refs := extractRefs(e.Payload)

	red, err := p.attribute(ctx, refs)
	if errors.Is(err, redemption.ErrNotFound) {
		return StatusIgnored, nil
	}
	if err != nil {
		return "", err
	}

	state, err := p.prov.VerifyRefund(ctx, provider.RefundRef{
		PaymentID:  refs.transactionID,
		InvoiceID:  refs.invoiceID,
		CustomerID: refs.customerID,
	})
	if err != nil {
		return "", fmt.Errorf("verify refund: %w", err)
	}
	if !state.Refunded {
		return "", errors.New("provider has not settled this refund yet")
	}

	return StatusProcessed, database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		locked, err := redemption.Lock(ctx, tx, red.ID)
		if err != nil {
			return err
		}
		if locked.Status == redemption.StatusRefunded {
			return nil
		}
		if locked.Status != redemption.StatusPurchaseConfirmed && locked.Status != redemption.StatusRefundPending {
			return fmt.Errorf("redemption %s cannot be refunded from state %s", locked.ID, locked.Status)
		}
		if err := redemption.MarkRefunded(ctx, tx, locked.ID); err != nil {
			return err
		}
		if err := recordTransaction(ctx, tx, locked, provider.PurchaseState{
			TransactionID: refs.transactionID, InvoiceID: refs.invoiceID,
			CustomerID: refs.customerID, Amount: state.Amount, Currency: state.Currency,
		}, "REFUND"); err != nil {
			return err
		}
		// The original CONSUME row stays; the restore is a new ledger entry.
		return wallet.Restore(ctx, tx, locked.UserID, locked.ID, locked.CoinCost)
	})
}

// attribute maps a provider event to a local redemption, preferring the unique
// coupon code because it is the strongest signal we issue.
func (p *Processor) attribute(ctx context.Context, refs providerRefs) (redemption.Redemption, error) {
	if refs.couponCode != "" {
		red, err := p.redeem.FindByCouponCode(ctx, p.prov.Code(), refs.couponCode)
		if err == nil || !errors.Is(err, redemption.ErrNotFound) {
			return red, err
		}
	}
	if refs.productID == "" || refs.planID == "" {
		return redemption.Redemption{}, redemption.ErrNotFound
	}
	return p.redeem.FindByProviderRefs(ctx, p.prov.Code(),
		refs.productID, refs.planID, refs.customerID, refs.invoiceID)
}

func recordTransaction(ctx context.Context, tx pgx.Tx, red redemption.Redemption, state provider.PurchaseState, kind string) error {
	if state.TransactionID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO provider_transactions
		(id, provider, provider_transaction_id, redemption_id, kind, invoice_id, customer_id,
		 amount, currency, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (provider, provider_transaction_id, kind) DO NOTHING`,
		uuid.New(), red.Provider, state.TransactionID, red.ID, kind,
		nilIfEmpty(state.InvoiceID), nilIfEmpty(state.CustomerID),
		int64(state.Amount), fallback(state.Currency, red.Currency), "SUCCESS")
	return err
}

func fallback(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
