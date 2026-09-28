package redemption

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"course-coupon/internal/auth"
	"course-coupon/internal/course"
	"course-coupon/internal/httpapi"
	"course-coupon/internal/idempotency"
	"course-coupon/internal/money"
)

const opClaim = "claim"

type Handler struct {
	svc  *Service
	keys *idempotency.Store
	log  *slog.Logger
}

func NewHandler(svc *Service, keys *idempotency.Store, log *slog.Logger) *Handler {
	return &Handler{svc: svc, keys: keys, log: log}
}

func (h *Handler) Routes(r chi.Router) {
	r.Post("/courses/{courseID}/claim", h.claim)
	r.Post("/redemptions/{redemptionID}/checkout", h.checkout)
	r.Post("/redemptions/{redemptionID}/opened", h.opened)
	r.Post("/redemptions/{redemptionID}/refund", h.refund)
	r.Get("/redemptions/{redemptionID}", h.get)
	r.Get("/me/course-history", h.history)
}

// View is the frontend shape of a redemption. Amounts are decimal major units.
type View struct {
	ID       uuid.UUID `json:"id"`
	Status   string    `json:"status"`
	CourseID uuid.UUID `json:"course_id"`
	Title    string    `json:"title"`
	PlanName string    `json:"plan_name"`
	Provider string    `json:"provider"`

	Pricing struct {
		MRP             *json.Number `json:"mrp,omitempty"`
		ProviderPrice   json.Number  `json:"provider_price"`
		DiscountPercent json.Number  `json:"discount_percent"`
		DiscountAmount  json.Number  `json:"discount_amount"`
		ExpectedPrice   json.Number  `json:"expected_checkout_price"`
		Currency        string       `json:"currency"`
	} `json:"pricing"`

	CoinCost int64 `json:"coin_cost"`

	Coupon *couponView `json:"coupon,omitempty"`

	CheckoutURL string `json:"checkout_url,omitempty"`
	// CouponPrefilled is false when the student must apply the code by hand.
	CouponPrefilled *bool `json:"coupon_prefilled,omitempty"`

	QuoteVerifiedAt     time.Time  `json:"quote_verified_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	PurchaseConfirmedAt *time.Time `json:"purchase_confirmed_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

type couponView struct {
	Code      string    `json:"code"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

func toView(res Result) View {
	r := res.Redemption
	v := View{
		ID: r.ID, Status: r.Status, CourseID: r.CourseID,
		Title: res.Course.Title, PlanName: res.Course.PlanName, Provider: r.Provider,
		CoinCost: r.CoinCost, QuoteVerifiedAt: r.QuoteVerifiedAt, ExpiresAt: r.ExpiresAt,
		PurchaseConfirmedAt: r.PurchaseConfirmedAt, CreatedAt: r.CreatedAt,
	}
	v.Pricing.ProviderPrice = r.ProviderPrice.Major()
	v.Pricing.DiscountPercent = money.Minor(r.DiscountBps).Major()
	v.Pricing.DiscountAmount = r.ExpectedDiscountAmount.Major()
	v.Pricing.ExpectedPrice = r.ExpectedCheckoutAmount.Major()
	v.Pricing.Currency = r.Currency
	if r.MRP != nil {
		m := r.MRP.Major()
		v.Pricing.MRP = &m
	}
	if res.Coupon != nil {
		v.Coupon = &couponView{Code: res.Coupon.Code, Status: res.Coupon.Status, ExpiresAt: res.Coupon.ExpiresAt}
	}
	if res.Checkout != nil {
		v.CheckoutURL = res.Checkout.URL
		prefilled := res.Checkout.CouponPrefilled
		v.CouponPrefilled = &prefilled
	}
	return v
}

func (h *Handler) claim(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())

	courseID, err := uuid.Parse(chi.URLParam(r, "courseID"))
	if err != nil {
		httpapi.Fail(w, r, h.log,
			httpapi.NewError(http.StatusNotFound, httpapi.CodeCourseNotFound, "course not found"))
		return
	}
	key, err := idempotency.Key(r)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	request := map[string]any{"course_id": courseID}
	replay, err := h.keys.Begin(r.Context(), userID, opClaim, key, request)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	// A retried claim returns the original coupon rather than issuing a second one.
	if replay != nil {
		w.Header().Set("Idempotent-Replay", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(replay.Response)
		return
	}

	res, err := h.svc.Claim(r.Context(), userID, courseID)
	if err != nil {
		h.keys.Release(r.Context(), userID, opClaim, key)
		httpapi.Fail(w, r, h.log, err)
		return
	}

	view := toView(res)
	if err := h.keys.Complete(r.Context(), h.svc.pool, userID, opClaim, key, res.Redemption.ID, view); err != nil {
		h.log.ErrorContext(r.Context(), "failed to persist idempotent claim result",
			"redemption_id", res.Redemption.ID, "error", err)
	}
	httpapi.JSON(w, http.StatusCreated, view)
}

type checkoutRequest struct {
	// AcceptPrice confirms a provider price the student has already been shown.
	AcceptPrice *json.Number `json:"accept_price,omitempty"`
}

func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())
	id, err := h.redemptionID(r)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	var body checkoutRequest
	if r.ContentLength > 0 {
		if err := httpapi.Decode(r, &body); err != nil {
			httpapi.Fail(w, r, h.log, err)
			return
		}
	}
	var accept *money.Minor
	if body.AcceptPrice != nil {
		parsed, err := money.ParseMajor(*body.AcceptPrice)
		if err != nil {
			httpapi.Fail(w, r, h.log, httpapi.NewError(http.StatusBadRequest,
				httpapi.CodeInvalidRequest, "accept_price is not a valid amount"))
			return
		}
		accept = &parsed
	}

	res, err := h.svc.Checkout(r.Context(), userID, id, accept)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toView(res))
}

func (h *Handler) opened(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())
	id, err := h.redemptionID(r)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	red, err := h.svc.MarkOpened(r.Context(), userID, id)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"id":                 red.ID,
		"status":             red.Status,
		"checkout_opened_at": red.CheckoutOpenedAt,
	})
}

func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())
	id, err := h.redemptionID(r)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	red, err := h.svc.RequestRefund(r.Context(), userID, id)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusAccepted, map[string]any{"id": red.ID, "status": red.Status})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())
	id, err := h.redemptionID(r)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	res, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toView(res))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())
	limit := clamp(queryInt(r, "limit", 20), 1, 100)
	page := clamp(queryInt(r, "page", 1), 1, 1_000_000)

	items, err := h.svc.redeem.History(r.Context(), userID, limit, (page-1)*limit)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	views := make([]View, 0, len(items))
	for _, red := range items {
		c, err := h.svc.courses.Get(r.Context(), red.CourseID)
		if err != nil {
			// A delisted course must not hide the student's purchase history.
			c = course.Course{Title: "Course unavailable"}
		}
		res := Result{Redemption: red, Course: c}
		if cpn, err := h.svc.coupons.ByRedemption(r.Context(), red.ID); err == nil {
			res.Coupon = &cpn
		}
		views = append(views, toView(res))
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"items": views, "page": page, "limit": limit})
}

func (h *Handler) redemptionID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "redemptionID"))
	if err != nil {
		return uuid.Nil, notFound()
	}
	return id, nil
}

func queryInt(r *http.Request, key string, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return n
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
