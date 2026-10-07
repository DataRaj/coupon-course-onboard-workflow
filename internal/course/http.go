package course

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"course-coupon/internal/httpapi"
	"course-coupon/internal/money"
)

// View is the frontend representation. Amounts are decimal major units; provider
// response blobs are never exposed.
type View struct {
	Catalog  *CatalogDetails `json:"catalog,omitempty"`
	ID       uuid.UUID       `json:"id"`
	Title    string          `json:"title"`
	PlanName string          `json:"plan_name"`
	Provider string          `json:"provider"`

	Class       *int32 `json:"class,omitempty"`
	Board       string `json:"board,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Language    string `json:"language,omitempty"`
	TargetExam  string `json:"target_exam,omitempty"`
	Thumbnail   string `json:"thumbnail_url,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Description string `json:"description,omitempty"`

	Pricing PricingView `json:"pricing"`
	Offer   *OfferView  `json:"offer,omitempty"`
}

type PricingView struct {
	ObservedAt   *time.Time   `json:"observed_at,omitempty"`
	Status       string       `json:"status,omitempty"`
	MRP          *json.Number `json:"mrp,omitempty"`
	SellingPrice json.Number  `json:"selling_price"`
	Currency     string       `json:"currency"`
}

type OfferView struct {
	DiscountPercent json.Number `json:"discount_percent"`
	CoinCost        int64       `json:"coin_cost"`
	ExpectedPrice   json.Number `json:"expected_price"`
}

func ToView(c Course, o *Offer) View {
	v := View{
		Catalog: c.Catalog, ID: c.ID, Title: c.Title, PlanName: c.PlanName, Provider: c.Provider,
		Class: c.Class, Board: c.Board, Subject: c.Subject, Language: c.Language,
		TargetExam: c.TargetExam, Thumbnail: c.Thumbnail, Duration: c.Duration,
		Description: c.Description,
		Pricing: PricingView{
			ObservedAt: c.CommercialObservedAt, Status: c.CommercialStatus,
			SellingPrice: c.SellingPrice.Major(),
			Currency:     c.Currency,
		},
	}
	if c.MRP != nil {
		m := c.MRP.Major()
		v.Pricing.MRP = &m
	}
	if o != nil {
		q := BuildQuote(c, *o, c.SellingPrice, c.MRP)
		v.Offer = &OfferView{
			DiscountPercent: bpsPercent(o.DiscountBps),
			CoinCost:        o.CoinCost,
			ExpectedPrice:   q.ExpectedCheckout.Major(),
		}
	}
	return v
}

func bpsPercent(bps int32) json.Number {
	return money.Minor(bps).Major()
}

type Handler struct {
	store *Store
	log   *slog.Logger
}

func NewHandler(store *Store, log *slog.Logger) *Handler {
	return &Handler{store: store, log: log}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/courses", h.list)
	r.Get("/courses/{courseID}", h.get)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{
		Page:       atoiDefault(q.Get("page"), 1),
		Limit:      atoiDefault(q.Get("limit"), 20),
		Board:      q.Get("board"),
		Subject:    q.Get("subject"),
		Language:   q.Get("language"),
		TargetExam: q.Get("target_exam"),
		Search:     q.Get("search"),
	}
	if v := q.Get("class"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			c := int32(n)
			f.Class = &c
		}
	}
	f.Normalize()

	courses, total, err := h.store.List(r.Context(), f)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	ids := make([]uuid.UUID, len(courses))
	for i, c := range courses {
		ids[i] = c.ID
	}
	offers, err := h.store.ActiveOffers(r.Context(), ids)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	views := make([]View, 0, len(courses))
	for _, c := range courses {
		var o *Offer
		if found, ok := offers[c.ID]; ok {
			o = &found
		}
		views = append(views, ToView(c, o))
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"items": views,
		"page":  f.Page,
		"limit": f.Limit,
		"total": total,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "courseID"))
	if err != nil {
		httpapi.Fail(w, r, h.log,
			httpapi.NewError(http.StatusNotFound, httpapi.CodeCourseNotFound, "course not found"))
		return
	}

	c, err := h.store.Get(r.Context(), id)
	if err != nil {
		httpapi.Fail(w, r, h.log, mapStoreErr(err))
		return
	}
	offer, ok, err := h.store.ActiveOffer(r.Context(), c.ID)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	var o *Offer
	if ok {
		o = &offer
	}
	httpapi.JSON(w, http.StatusOK, ToView(c, o))
}

func mapStoreErr(err error) error {
	if err == ErrNotFound {
		return httpapi.NewError(http.StatusNotFound, httpapi.CodeCourseNotFound, "course not found")
	}
	return err
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
