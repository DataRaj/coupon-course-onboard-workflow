// Package provider defines the marketplace-facing boundary that every external
// course-commerce platform must implement. Marketplace services talk only to this
// interface; provider-specific HTTP details stay inside the adapter packages.
package provider

import (
	"context"
	"errors"
	"time"

	"course-coupon/internal/money"
)

const Pabbly = "PABBLY"

// CourseRef identifies one sellable Product+Plan pair at the provider.
type CourseRef struct {
	ProductID string
	PlanID    string
}

// CatalogItem is a normalized Product+Plan pair. One provider product with several
// active plans yields several CatalogItems.
type CatalogItem struct {
	Ref         CourseRef
	Title       string
	PlanName    string
	Description string

	SellingPrice money.Minor
	MRP          *money.Minor
	Currency     string

	CheckoutURL string
	Active      bool

	// Academic attributes read from provider plan metadata; all optional.
	Class      *int32
	Board      string
	Subject    string
	Language   string
	TargetExam string
	Thumbnail  string
	Duration   string

	ProviderCreatedAt *time.Time
	ProviderUpdatedAt *time.Time
}

// CommercialState is the live, authoritative price for a course at claim/checkout time.
type CommercialState struct {
	SellingPrice money.Minor
	MRP          *money.Minor
	Currency     string
	Active       bool
	CheckoutURL  string
	FetchedAt    time.Time
}

type CouponRequest struct {
	Ref         CourseRef
	Name        string
	Code        string
	DiscountBps int32
	ValidUpto   time.Time
}

type ProviderCoupon struct {
	ID   string
	Code string
}

type Checkout struct {
	URL string
	// CouponPrefilled reports whether URL already carries the coupon. When false the
	// caller must surface the code for manual entry rather than pretend it is applied.
	CouponPrefilled bool
}

type PurchaseRef struct {
	InvoiceID     string
	CustomerID    string
	TransactionID string
	Ref           CourseRef
}

type PurchaseState struct {
	Paid          bool
	Failed        bool
	TransactionID string
	InvoiceID     string
	CustomerID    string
	Amount        money.Minor
	Currency      string
	CouponCode    string
	ProductID     string
	PlanID        string
}

type RefundRef struct {
	PaymentID  string
	InvoiceID  string
	CustomerID string
}

type RefundState struct {
	Refunded bool
	Amount   money.Minor
	Currency string
}

// Capabilities documents what a provider actually supports so callers never invoke
// a method the provider cannot honour.
type Capabilities struct {
	CatalogWebhook    bool
	LivePriceLookup   bool
	CouponCreate      bool
	CouponDisable     bool
	CheckoutLink      bool
	PurchaseWebhook   bool
	TransactionLookup bool
	RefundWebhook     bool
}

type Provider interface {
	Code() string
	Capabilities() Capabilities

	ListCatalog(ctx context.Context) ([]CatalogItem, error)
	GetCommercialState(ctx context.Context, ref CourseRef) (CommercialState, error)

	CreateCoupon(ctx context.Context, req CouponRequest) (ProviderCoupon, error)
	FindCouponByCode(ctx context.Context, productID, code string) (ProviderCoupon, error)
	DisableCoupon(ctx context.Context, productID, couponID string) error

	GetCheckout(ctx context.Context, ref CourseRef, couponCode string) (Checkout, error)

	VerifyPurchase(ctx context.Context, ref PurchaseRef) (PurchaseState, error)
	VerifyRefund(ctx context.Context, ref RefundRef) (RefundState, error)
}

var (
	// ErrUnavailable means the provider could not be reached or returned a transient
	// failure; the caller must not commit money decisions on stale data.
	ErrUnavailable = errors.New("provider temporarily unavailable")
	ErrNotFound    = errors.New("provider resource not found")
	// ErrUnsupported is returned by adapters for capabilities they do not have.
	ErrUnsupported = errors.New("operation not supported by provider")
)
