// Package redemption owns the marketplace business workflow. A Redemption — not a
// coupon — is the transaction object that ties a student, a course, a frozen price
// quote and the reserved coins together.
package redemption

import (
	"time"

	"github.com/google/uuid"

	"course-coupon/internal/money"
)

const (
	StatusCoinsReserved     = "COINS_RESERVED"
	StatusCouponCreating    = "COUPON_CREATING"
	StatusCouponIssued      = "COUPON_ISSUED"
	StatusCheckoutOpened    = "CHECKOUT_OPENED"
	StatusPurchasePending   = "PURCHASE_PENDING"
	StatusPurchaseConfirmed = "PURCHASE_CONFIRMED"
	StatusPaymentFailed     = "PAYMENT_FAILED"
	StatusExpired           = "EXPIRED"
	StatusProviderError     = "PROVIDER_ERROR"
	StatusRefundPending     = "REFUND_PENDING"
	StatusRefunded          = "REFUNDED"
)

// liveStatuses are the states that still hold reserved coins.
var liveStatuses = []string{
	StatusCoinsReserved, StatusCouponCreating, StatusCouponIssued,
	StatusCheckoutOpened, StatusPurchasePending,
}

type Redemption struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	CourseID uuid.UUID
	OfferID  uuid.UUID

	Provider          string
	ProviderProductID string
	ProviderPlanID    string

	PriceSnapshotID *uuid.UUID

	MRP                    *money.Minor
	ProviderPrice          money.Minor
	DiscountBps            int32
	CoinCost               int64
	ExpectedDiscountAmount money.Minor
	ExpectedCheckoutAmount money.Minor
	Currency               string

	Status   string
	CouponID *uuid.UUID

	ProviderCustomerID    *string
	ProviderInvoiceID     *string
	ProviderTransactionID *string

	QuoteVerifiedAt time.Time
	ExpiresAt       time.Time

	CheckoutOpenedAt    *time.Time
	PurchaseConfirmedAt *time.Time
	RefundedAt          *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (r Redemption) Expired(now time.Time) bool { return now.After(r.ExpiresAt) }

// HoldsCoins reports whether this redemption still has coins reserved.
func (r Redemption) HoldsCoins() bool {
	for _, s := range liveStatuses {
		if r.Status == s {
			return true
		}
	}
	return false
}
