// Package admin exposes small debug/operator endpoints. No product/plan CRUD, no
// course CRUD — only triggers for work the worker already does on a schedule.
package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"course-coupon/internal/httpapi"
	"course-coupon/internal/redemption"
	catalogsync "course-coupon/internal/sync"
	"course-coupon/internal/webhook"
)

type Handler struct {
	catalog   *catalogsync.Catalog
	processor *webhook.Processor
	svc       *redemption.Service
	log       *slog.Logger
}

func NewHandler(catalog *catalogsync.Catalog, processor *webhook.Processor, svc *redemption.Service, log *slog.Logger) *Handler {
	return &Handler{catalog: catalog, processor: processor, svc: svc, log: log}
}

func (h *Handler) Routes(r chi.Router) {
	r.Post("/admin/providers/pabbly/sync", h.sync)
	r.Post("/admin/webhooks/process", h.processWebhooks)
	r.Post("/admin/redemptions/{id}/reconcile", h.reconcile)
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	if err := h.catalog.Run(r.Context()); err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) processWebhooks(w http.ResponseWriter, r *http.Request) {
	n, err := h.processor.ProcessBatch(r.Context(), 50)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"processed": n})
}

// reconcile re-verifies one redemption against the provider directly, for the case
// where a webhook was missed or is still stuck retrying.
func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Fail(w, r, h.log,
			httpapi.NewError(http.StatusNotFound, httpapi.CodeNotFound, "redemption not found"))
		return
	}
	status, err := h.svc.Reconcile(r.Context(), id)
	if err != nil {
		httpapi.Fail(w, r, h.log, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}
