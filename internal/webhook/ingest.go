// Package webhook accepts provider notifications and processes them out of band.
// A webhook is only a hint: payment is confirmed by re-reading the provider API.
package webhook

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/httpapi"
	"course-coupon/internal/provider"
)

const maxWebhookBody = 1 << 20

// forwardedHeaders are kept for debugging; credentials and auth headers are not.
var forwardedHeaders = []string{"Content-Type", "User-Agent", "X-Request-Id", "X-Pabbly-Event"}

type Ingest struct {
	pool   *pgxpool.Pool
	secret string
	log    *slog.Logger
}

func NewIngest(pool *pgxpool.Pool, secret string, log *slog.Logger) *Ingest {
	return &Ingest{pool: pool, secret: secret, log: log.With("component", "webhook")}
}

func (i *Ingest) Routes(r chi.Router) {
	r.Post("/webhooks/pabbly/{secret}", i.receive)
}

// receive persists the raw event and returns immediately. No provider reconciliation
// happens before acknowledging.
func (i *Ingest) receive(w http.ResponseWriter, r *http.Request) {
	given := chi.URLParam(r, "secret")
	if subtle.ConstantTimeCompare([]byte(given), []byte(i.secret)) != 1 {
		http.NotFound(w, r)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		httpapi.JSON(w, http.StatusRequestEntityTooLarge, map[string]string{"status": "rejected"})
		return
	}
	if !json.Valid(raw) {
		httpapi.JSON(w, http.StatusBadRequest, map[string]string{"status": "invalid_json"})
		return
	}

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	eventType, eventID := identify(payload)

	headers := map[string]string{}
	for _, h := range forwardedHeaders {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	headerJSON, _ := json.Marshal(headers)

	// The payload hash deduplicates redelivery of the identical event.
	_, err = i.pool.Exec(r.Context(), `INSERT INTO provider_webhook_events
		(id, provider, provider_event_id, event_type, payload_hash, payload, headers)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider, payload_hash) DO NOTHING`,
		uuid.New(), provider.Pabbly, nilIfEmpty(eventID), eventType, hash, raw, headerJSON)
	if err != nil {
		i.log.ErrorContext(r.Context(), "failed to persist webhook", "error", err, "event_type", eventType)
		httpapi.JSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}

	i.log.InfoContext(r.Context(), "webhook received", "event_type", eventType)
	httpapi.JSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// identify extracts the event discriminator, tolerating Pabbly's varying envelopes.
func identify(payload map[string]any) (eventType, eventID string) {
	for _, key := range []string{"event_type", "event", "type", "webhook_event"} {
		if v, ok := payload[key].(string); ok && v != "" {
			eventType = v
			break
		}
	}
	if eventType == "" {
		eventType = "UNKNOWN"
	}
	for _, key := range []string{"event_id", "id", "_id"} {
		if v, ok := payload[key].(string); ok && v != "" {
			eventID = v
			break
		}
	}
	return eventType, eventID
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
