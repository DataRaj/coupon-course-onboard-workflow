package redemption

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/config"
	"course-coupon/internal/coupon"
	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"course-coupon/internal/httpapi"
	"course-coupon/internal/money"
	"course-coupon/internal/provider"
	"course-coupon/internal/wallet"
)

type Service struct {
	pool    *pgxpool.Pool
	courses *course.Store
	coupons *coupon.Store
	redeem  *Store
	prov    provider.Provider
	cfg     config.Config
	log     *slog.Logger
}

func NewService(pool *pgxpool.Pool, courses *course.Store, coupons *coupon.Store, redeem *Store, prov provider.Provider, cfg config.Config, log *slog.Logger) *Service {
	return &Service{pool: pool, courses: courses, coupons: coupons, redeem: redeem,
		prov: prov, cfg: cfg, log: log.With("component", "redemption")}
}

// Result is what the claim and checkout endpoints hand back to the frontend.
type Result struct {
	Redemption Redemption
	Coupon     *coupon.Coupon
	Course     course.Course
	Checkout   *provider.Checkout
}

// Claim reserves coins against a freshly revalidated provider price and issues one
// unique, plan-scoped coupon. Coins are reserved, never consumed, at this stage.
func (s *Service) Claim(ctx context.Context, userID, courseID uuid.UUID) (Result, error) {
	c, err := s.courses.Get(ctx, courseID)
	if err != nil {
		if errors.Is(err, course.ErrNotFound) {
			return Result{}, httpapi.NewError(http.StatusNotFound, httpapi.CodeCourseNotFound, "course not found")
		}
		return Result{}, err
	}

	offer, ok, err := s.courses.ActiveOffer(ctx, c.ID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeOfferNotAvailable,
			"no active reward for this course")
	}

	// The provider is the price authority; the frontend's number is never trusted.
	state, err := s.liveState(ctx, c)
	if err != nil {
		return Result{}, err
	}
	if !state.Active {
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeCourseUnavailable,
			"this course is no longer available from the provider")
	}

	now := time.Now().UTC()
	var created Redemption

	// Reserve locally and commit before touching the network: no Pabbly call may
	// happen while a wallet row lock is held.
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		snapshotID, _, err := s.courses.ApplyLivePrice(ctx, tx, c, state, course.SourceClaimRevalidation)
		if err != nil {
			return err
		}
		quote := course.BuildQuote(c, offer, state.SellingPrice, state.MRP)

		created = Redemption{
			ID: uuid.New(), UserID: userID, CourseID: c.ID, OfferID: offer.ID,
			Provider:          s.prov.Code(),
			ProviderProductID: c.ProviderProductID,
			ProviderPlanID:    c.ProviderPlanID,
			MRP:               quote.MRP,
			ProviderPrice:     quote.ProviderPrice,
			DiscountBps:       quote.DiscountBps,
			CoinCost:          quote.CoinCost,

			ExpectedDiscountAmount: quote.DiscountAmount,
			ExpectedCheckoutAmount: quote.ExpectedCheckout,
			Currency:               quote.Currency,

			Status:          StatusCoinsReserved,
			QuoteVerifiedAt: state.FetchedAt,
			ExpiresAt:       now.Add(s.cfg.RedemptionTTL),
		}
		if snapshotID != uuid.Nil {
			created.PriceSnapshotID = &snapshotID
		}

		if err := s.redeem.Insert(ctx, tx, created); err != nil {
			return err
		}
		return wallet.Reserve(ctx, tx, userID, created.ID, offer.CoinCost)
	})
	switch {
	case errors.Is(err, ErrActiveExists):
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeActiveRedemption,
			"you already have an active reward for this course")
	case err != nil:
		return Result{}, err
	}

	issued, err := s.issueCoupon(ctx, created, c)
	if err != nil {
		return Result{}, err
	}

	created.Status = StatusCouponIssued
	created.CouponID = &issued.ID
	c.SellingPrice, c.MRP, c.Currency = state.SellingPrice, state.MRP, state.Currency
	return Result{Redemption: created, Coupon: &issued, Course: c}, nil
}

// issueCoupon creates the provider coupon outside any database transaction and
// releases the reserved coins if creation conclusively failed.
func (s *Service) issueCoupon(ctx context.Context, r Redemption, c course.Course) (coupon.Coupon, error) {
	code, err := coupon.GenerateCode()
	if err != nil {
		return coupon.Coupon{}, err
	}

	local := coupon.Coupon{
		ID: uuid.New(), RedemptionID: r.ID, Provider: s.prov.Code(), Code: code,
		DiscountBps: r.DiscountBps, Status: coupon.StatusCreating,
		ProviderProductID: r.ProviderProductID, ProviderPlanID: r.ProviderPlanID,
		ExpiresAt: r.ExpiresAt,
	}
	// Persist the code before calling out so an ambiguous timeout is recoverable.
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.coupons.Create(ctx, tx, local); err != nil {
			return err
		}
		return AttachCoupon(ctx, tx, r.ID, local.ID, StatusCouponCreating)
	}); err != nil {
		return coupon.Coupon{}, err
	}

	req := provider.CouponRequest{
		Ref:         provider.CourseRef{ProductID: r.ProviderProductID, PlanID: r.ProviderPlanID},
		Name:        "Marketplace reward — " + c.Title,
		Code:        code,
		DiscountBps: r.DiscountBps,
		// Pabbly's validity is date-oriented; the shorter local TTL stays authoritative.
		ValidUpto: r.ExpiresAt.Add(24 * time.Hour),
	}

	remote, err := s.prov.CreateCoupon(ctx, req)
	if err != nil {
		// A timeout may have still created the coupon, so look before retrying.
		if found, lookupErr := s.prov.FindCouponByCode(ctx, r.ProviderProductID, code); lookupErr == nil {
			remote, err = found, nil
		} else {
			s.log.WarnContext(ctx, "coupon creation failed", "redemption_id", r.ID, "error", err)
			return coupon.Coupon{}, s.abandon(ctx, r, local, err)
		}
	}

	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.coupons.MarkIssued(ctx, tx, local.ID, remote.ID); err != nil {
			return err
		}
		return SetStatus(ctx, tx, r.ID, StatusCouponIssued)
	}); err != nil {
		return coupon.Coupon{}, err
	}

	local.Status = coupon.StatusActive
	local.ProviderCouponID = &remote.ID
	return local, nil
}

// abandon releases the reservation after a conclusive provider failure.
func (s *Service) abandon(ctx context.Context, r Redemption, local coupon.Coupon, cause error) error {
	releaseErr := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := Lock(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if !locked.HoldsCoins() {
			return nil
		}
		if err := s.coupons.SetStatus(ctx, tx, local.ID, coupon.StatusFailed); err != nil {
			return err
		}
		if err := SetStatus(ctx, tx, r.ID, StatusProviderError); err != nil {
			return err
		}
		return wallet.Release(ctx, tx, r.UserID, r.ID, r.CoinCost)
	})
	if releaseErr != nil {
		s.log.ErrorContext(ctx, "failed to release coins after coupon failure",
			"redemption_id", r.ID, "error", releaseErr)
	}
	return httpapi.NewError(http.StatusBadGateway, httpapi.CodeCouponCreationFailed,
		"could not issue your reward coupon; your coins were not spent").WithCause(cause)
}

// liveState fetches the authoritative provider price, refusing to commit against
// arbitrarily stale cached data when the provider is unreachable.
func (s *Service) liveState(ctx context.Context, c course.Course) (provider.CommercialState, error) {
	state, err := s.prov.GetCommercialState(ctx,
		provider.CourseRef{ProductID: c.ProviderProductID, PlanID: c.ProviderPlanID})
	if err == nil {
		return state, nil
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		return provider.CommercialState{}, err
	}

	if time.Since(c.LastSyncedAt) > s.cfg.CatalogHardStaleAfter {
		return provider.CommercialState{}, httpapi.NewError(http.StatusServiceUnavailable,
			httpapi.CodeProviderUnavailable,
			"course pricing could not be verified right now; please try again shortly").WithCause(err)
	}
	s.log.WarnContext(ctx, "using recently synced price while provider is unreachable",
		"course_id", c.ID, "last_synced_at", c.LastSyncedAt)
	return provider.CommercialState{
		SellingPrice: c.SellingPrice, MRP: c.MRP, Currency: c.Currency,
		Active: c.ProviderActive, CheckoutURL: c.CheckoutURL, FetchedAt: c.LastSyncedAt,
	}, nil
}

// Checkout revalidates the quote if it has aged past PRICE_QUOTE_MAX_AGE and returns
// the provider checkout page plus the coupon code.
func (s *Service) Checkout(ctx context.Context, userID, redemptionID uuid.UUID, acceptPrice *money.Minor) (Result, error) {
	r, err := s.owned(ctx, userID, redemptionID)
	if err != nil {
		return Result{}, err
	}
	now := time.Now().UTC()

	switch {
	case r.Status != StatusCouponIssued && r.Status != StatusCheckoutOpened:
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeInvalidState,
			"this reward is not ready for checkout")
	case r.Expired(now):
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeRedemptionExpired,
			"this reward has expired")
	}

	cpn, err := s.coupons.ByRedemption(ctx, r.ID)
	if err != nil {
		return Result{}, err
	}
	if cpn.Status != coupon.StatusActive {
		return Result{}, httpapi.NewError(http.StatusConflict, httpapi.CodeInvalidState,
			"this reward coupon is not active")
	}

	c, err := s.courses.Get(ctx, r.CourseID)
	if err != nil {
		return Result{}, err
	}

	if now.Sub(r.QuoteVerifiedAt) > s.cfg.PriceQuoteMaxAge {
		r, err = s.revalidate(ctx, r, c, acceptPrice)
		if err != nil {
			return Result{}, err
		}
	}

	checkout, err := s.prov.GetCheckout(ctx,
		provider.CourseRef{ProductID: r.ProviderProductID, PlanID: r.ProviderPlanID}, cpn.Code)
	if err != nil {
		return Result{}, httpapi.NewError(http.StatusServiceUnavailable,
			httpapi.CodeProviderUnavailable, "checkout is temporarily unavailable").WithCause(err)
	}

	return Result{Redemption: r, Coupon: &cpn, Course: c, Checkout: &checkout}, nil
}

// revalidate re-reads the provider price and, when it moved, asks the frontend to
// confirm the new economics rather than silently charging the stale amount.
func (s *Service) revalidate(ctx context.Context, r Redemption, c course.Course, acceptPrice *money.Minor) (Redemption, error) {
	state, err := s.liveState(ctx, c)
	if err != nil {
		return r, err
	}

	previous := r.ProviderPrice
	discount := state.SellingPrice.ApplyBps(r.DiscountBps)
	checkout := state.SellingPrice - discount

	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		snapshotID, _, err := s.courses.ApplyLivePrice(ctx, tx, c, state, course.SourceCheckoutRevalidation)
		if err != nil {
			return err
		}
		var snap *uuid.UUID
		if snapshotID != uuid.Nil {
			snap = &snapshotID
		}
		return UpdateQuote(ctx, tx, r.ID, snap, state.MRP, state.SellingPrice, discount, checkout, state.FetchedAt)
	})
	if err != nil {
		return r, err
	}

	r.ProviderPrice, r.MRP = state.SellingPrice, state.MRP
	r.ExpectedDiscountAmount, r.ExpectedCheckoutAmount = discount, checkout
	r.QuoteVerifiedAt = state.FetchedAt

	if previous != state.SellingPrice && (acceptPrice == nil || *acceptPrice != state.SellingPrice) {
		return r, httpapi.NewError(http.StatusConflict, httpapi.CodePriceChanged,
			"the provider price changed; please confirm the new amount").
			WithDetails(map[string]any{
				"previous_price":          previous.Major(),
				"current_price":           state.SellingPrice.Major(),
				"expected_checkout_price": checkout.Major(),
				"currency":                state.Currency,
			})
	}
	return r, nil
}

// MarkOpened records that the student left for the provider checkout page. It is
// idempotent and never consumes coins.
func (s *Service) MarkOpened(ctx context.Context, userID, redemptionID uuid.UUID) (Redemption, error) {
	r, err := s.owned(ctx, userID, redemptionID)
	if err != nil {
		return r, err
	}
	if r.Status == StatusCheckoutOpened {
		return r, nil
	}
	if r.Status != StatusCouponIssued {
		return r, httpapi.NewError(http.StatusConflict, httpapi.CodeInvalidState,
			"this reward is not ready for checkout")
	}
	if err := MarkCheckoutOpened(ctx, s.pool, r.ID); err != nil {
		return r, err
	}
	return s.redeem.Get(ctx, r.ID)
}

func (s *Service) Get(ctx context.Context, userID, redemptionID uuid.UUID) (Result, error) {
	r, err := s.owned(ctx, userID, redemptionID)
	if err != nil {
		return Result{}, err
	}
	c, err := s.courses.Get(ctx, r.CourseID)
	if err != nil {
		return Result{}, err
	}
	res := Result{Redemption: r, Course: c}
	if cpn, err := s.coupons.ByRedemption(ctx, r.ID); err == nil {
		res.Coupon = &cpn
	}
	return res, nil
}

func (s *Service) owned(ctx context.Context, userID, redemptionID uuid.UUID) (Redemption, error) {
	r, err := s.redeem.Get(ctx, redemptionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return r, notFound()
		}
		return r, err
	}
	if r.UserID != userID {
		// Do not disclose that someone else's redemption exists.
		return Redemption{}, notFound()
	}
	return r, nil
}

func notFound() error {
	return httpapi.NewError(http.StatusNotFound, httpapi.CodeNotFound, "redemption not found")
}
