package redemption

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"course-coupon/internal/database"
	"course-coupon/internal/httpapi"
)

// RequestRefund asks the provider to refund the confirmed payment. The actual coin
// restoration happens only once the Payment Refund webhook is verified — this call
// just starts the provider-side refund and marks the redemption as pending.
func (s *Service) RequestRefund(ctx context.Context, userID, redemptionID uuid.UUID) (Redemption, error) {
	r, err := s.owned(ctx, userID, redemptionID)
	if err != nil {
		return r, err
	}
	if r.Status != StatusPurchaseConfirmed {
		return r, httpapi.NewError(http.StatusConflict, httpapi.CodeInvalidState,
			"only a confirmed purchase can be refunded")
	}
	if r.ProviderTransactionID == nil || *r.ProviderTransactionID == "" {
		return r, httpapi.NewError(http.StatusConflict, httpapi.CodeInvalidState,
			"no provider transaction is recorded for this redemption")
	}

	refunder, ok := s.prov.(interface {
		RequestRefund(context.Context, string) error
	})
	if !ok {
		return r, httpapi.NewError(http.StatusNotImplemented, httpapi.CodeInvalidState,
			"the provider does not support refund requests")
	}
	if err := refunder.RequestRefund(ctx, *r.ProviderTransactionID); err != nil {
		return r, httpapi.NewError(http.StatusBadGateway, httpapi.CodeProviderUnavailable,
			"could not start the refund with the provider").WithCause(err)
	}

	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := Lock(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if locked.Status != StatusPurchaseConfirmed {
			return nil
		}
		return SetStatus(ctx, tx, locked.ID, StatusRefundPending)
	}); err != nil {
		return r, err
	}

	r.Status = StatusRefundPending
	return r, nil
}
