package course

import (
	"time"

	"github.com/google/uuid"

	"course-coupon/internal/money"
)

// Course is the local projection of one provider Product+Plan pair. It is a read
// model: the provider stays the source of truth for price and availability.
type Course struct {
	Catalog              *CatalogDetails
	CommercialObservedAt *time.Time
	CommercialStatus     string
	ID                   uuid.UUID
	Provider             string
	ProviderProductID    string
	ProviderPlanID       string

	Title       string
	PlanName    string
	Description string

	Class      *int32
	Board      string
	Subject    string
	Language   string
	TargetExam string
	Thumbnail  string
	Duration   string

	MRP          *money.Minor
	SellingPrice money.Minor
	Currency     string

	CheckoutURL    string
	ProviderActive bool

	LastSyncedAt time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Offer is a platform-owned reward attached to a course. It is not provider data.
type Offer struct {
	ID          uuid.UUID
	CourseID    uuid.UUID
	DiscountBps int32
	CoinCost    int64
	StartsAt    time.Time
	EndsAt      *time.Time
	Status      string
}

// Snapshot source values.
const (
	SourceSync                 = "SYNC"
	SourceClaimRevalidation    = "CLAIM_REVALIDATION"
	SourceCheckoutRevalidation = "CHECKOUT_REVALIDATION"
	SourceReconciliation       = "RECONCILIATION"
)

// Quote is the reward maths for one course+offer at a known provider price.
type Quote struct {
	MRP              *money.Minor
	ProviderPrice    money.Minor
	DiscountBps      int32
	CoinCost         int64
	DiscountAmount   money.Minor
	ExpectedCheckout money.Minor
	Currency         string
}

// BuildQuote derives the reward from the live provider price, never from the client.
func BuildQuote(c Course, o Offer, providerPrice money.Minor, mrp *money.Minor) Quote {
	discount := providerPrice.ApplyBps(o.DiscountBps)
	return Quote{
		MRP:              mrp,
		ProviderPrice:    providerPrice,
		DiscountBps:      o.DiscountBps,
		CoinCost:         o.CoinCost,
		DiscountAmount:   discount,
		ExpectedCheckout: providerPrice - discount,
		Currency:         c.Currency,
	}
}

// ListFilter drives GET /courses.
type ListFilter struct {
	Page       int
	Limit      int
	Class      *int32
	Board      string
	Subject    string
	Language   string
	TargetExam string
	Search     string
}

func (f *ListFilter) Normalize() {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 20
	}
}

func (f ListFilter) Offset() int { return (f.Page - 1) * f.Limit }
