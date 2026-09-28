package redemption

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"course-coupon/internal/coupon"
	"course-coupon/internal/database"
	"course-coupon/internal/httpapi"
	"course-coupon/internal/provider"
	"course-coupon/internal/wallet"
)

// Reconcile re-verifies an uncertain redemption straight against the provider,
// independent of the webhook queue. Used when a webhook was missed or delayed.
func (s *Service) Reconcile(ctx context.Context, redemptionID uuid.UUID) (string, error) {
	r, err := s.redeem.Get(ctx, redemptionID)
	if err != nil {
		return "", err
	}
	if !r.HoldsCoins() {
		return r.Status, nil
	}

	state, err := s.prov.VerifyPurchase(ctx, provider.PurchaseRef{
		InvoiceID:  derefStr(r.ProviderInvoiceID),
		CustomerID: derefStr(r.ProviderCustomerID),
		Ref:        provider.CourseRef{ProductID: r.ProviderProductID, PlanID: r.ProviderPlanID},
	})
	if err != nil {
		return "", httpapi.NewError(http.StatusBadGateway, httpapi.CodeProviderUnavailable,
			"could not verify this redemption with the provider").WithCause(err)
	}

	switch {
	case state.Paid:
		return StatusPurchaseConfirmed, database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			locked, err := Lock(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			if locked.Status == StatusPurchaseConfirmed || !locked.HoldsCoins() {
				return nil
			}
			if err := ConfirmPurchase(ctx, tx, locked.ID, state.CustomerID, state.InvoiceID, state.TransactionID); err != nil {
				return err
			}
			if locked.CouponID != nil {
				if err := s.coupons.SetStatus(ctx, tx, *locked.CouponID, coupon.StatusUsed); err != nil {
					return err
				}
			}
			return wallet.Consume(ctx, tx, locked.UserID, locked.ID, locked.CoinCost)
		})
	case state.Failed:
		return StatusPaymentFailed, database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			locked, err := Lock(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			if !locked.HoldsCoins() {
				return nil
			}
			if err := SetStatus(ctx, tx, locked.ID, StatusPaymentFailed); err != nil {
				return err
			}
			return wallet.Release(ctx, tx, locked.UserID, locked.ID, locked.CoinCost)
		})
	default:
		return r.Status, nil
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
