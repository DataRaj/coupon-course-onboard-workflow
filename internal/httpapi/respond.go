// Package httpapi holds transport-level concerns only: decoding, error mapping and
// encoding. Marketplace workflow lives in the domain services.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Stable machine-readable error codes. The frontend keys off these, never off text.
const (
	CodeCourseNotFound       = "COURSE_NOT_FOUND"
	CodeCourseUnavailable    = "COURSE_UNAVAILABLE"
	CodeOfferNotAvailable    = "OFFER_NOT_AVAILABLE"
	CodeInsufficientCoins    = "INSUFFICIENT_COINS"
	CodeActiveRedemption     = "ACTIVE_REDEMPTION_EXISTS"
	CodePriceChanged         = "PRICE_CHANGED"
	CodeProviderUnavailable  = "PROVIDER_UNAVAILABLE"
	CodeCouponCreationFailed = "COUPON_CREATION_FAILED"
	CodeRedemptionExpired    = "REDEMPTION_EXPIRED"
	CodeInvalidState         = "INVALID_REDEMPTION_STATE"
	CodePurchaseNotConfirmed = "PURCHASE_NOT_CONFIRMED"
	CodeInvalidRequest       = "INVALID_REQUEST"
	CodeIdempotencyConflict  = "IDEMPOTENCY_KEY_REUSED"
	CodeUnauthorized         = "UNAUTHORIZED"
	CodeNotFound             = "NOT_FOUND"
	CodeInternal             = "INTERNAL_ERROR"
)

// Error is a domain error carrying the frontend-visible code and HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
	// Details is merged into the response body, e.g. price-change fields.
	Details map[string]any
	cause   error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func (e *Error) Unwrap() error { return e.cause }

func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }
func (e *Error) WithCause(err error) *Error          { e.cause = err; return e }

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// Fail maps a domain error to a response. Provider internals are never leaked.
func Fail(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var domain *Error
	if !errors.As(err, &domain) {
		log.ErrorContext(r.Context(), "unhandled error", "error", err, "path", r.URL.Path)
		domain = NewError(http.StatusInternalServerError, CodeInternal, "something went wrong")
	}
	if domain.Status >= 500 {
		log.ErrorContext(r.Context(), "request failed", "code", domain.Code, "error", err, "path", r.URL.Path)
	}

	body := map[string]any{"code": domain.Code, "message": domain.Message}
	for k, v := range domain.Details {
		body[k] = v
	}
	JSON(w, domain.Status, body)
}

func Decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return NewError(http.StatusBadRequest, CodeInvalidRequest, "malformed request body").WithCause(err)
	}
	return nil
}
