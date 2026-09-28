// Package idempotency makes mutating endpoints safe under frontend retries.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/database"
	"course-coupon/internal/httpapi"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Replay is a previously completed response for the same key.
type Replay struct {
	ResourceID uuid.UUID
	Response   json.RawMessage
}

var errKeyReused = httpapi.NewError(http.StatusConflict, httpapi.CodeIdempotencyConflict,
	"this Idempotency-Key was already used with a different request")

// Begin claims the key. It returns a replay when the identical request already
// completed, and an error when the same key is reused for a different request.
func (s *Store) Begin(ctx context.Context, userID uuid.UUID, operation, key string, request any) (*Replay, error) {
	hash, err := hashRequest(request)
	if err != nil {
		return nil, err
	}

	var (
		storedHash string
		resourceID *uuid.UUID
		response   []byte
	)
	err = s.pool.QueryRow(ctx, `INSERT INTO idempotency_keys (user_id, operation, key, request_hash)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id, operation, key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, resource_id, response_json`,
		userID, operation, key, hash).Scan(&storedHash, &resourceID, &response)
	if err != nil {
		return nil, err
	}

	if storedHash != hash {
		return nil, errKeyReused
	}
	if resourceID == nil {
		// First caller, or a previous attempt that never completed; let it run again.
		return nil, nil
	}
	return &Replay{ResourceID: *resourceID, Response: response}, nil
}

// Complete stores the result so a retry returns the original outcome.
func (s *Store) Complete(ctx context.Context, q database.Execer, userID uuid.UUID, operation, key string, resourceID uuid.UUID, response any) error {
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `UPDATE idempotency_keys SET resource_id = $4, response_json = $5
		WHERE user_id = $1 AND operation = $2 AND key = $3`,
		userID, operation, key, resourceID, body)
	return err
}

// Release drops an unfinished key so a genuine retry can start over.
func (s *Store) Release(ctx context.Context, userID uuid.UUID, operation, key string) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM idempotency_keys
		WHERE user_id = $1 AND operation = $2 AND key = $3 AND resource_id IS NULL`,
		userID, operation, key)
}

func hashRequest(request any) (string, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Key reads and validates the required header.
func Key(r *http.Request) (string, error) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 200 {
		return "", httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"Idempotency-Key header is required")
	}
	return key, nil
}
