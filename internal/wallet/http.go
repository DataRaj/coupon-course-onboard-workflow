package wallet

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"course-coupon/internal/auth"
	"course-coupon/internal/httpapi"
)

type Handler struct {
	store *Store
	log   *slog.Logger
}

func NewHandler(store *Store, log *slog.Logger) *Handler {
	return &Handler{store: store, log: log}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/me/wallet", h.get)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserID(r.Context())

	account, err := h.store.Get(r.Context(), userID)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	ledger, err := h.store.Ledger(r.Context(), userID, 50)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"available_coins": account.Available,
		"reserved_coins":  account.Reserved,
		"ledger":          ledger,
	})
}
