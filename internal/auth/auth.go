// Package auth resolves the marketplace caller from the surrounding application's
// existing JWT. It deliberately provides no login/register of its own.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"course-coupon/internal/config"
	"course-coupon/internal/httpapi"
)

type ctxKey struct{}

type Authenticator struct {
	secret  []byte
	devMode bool
	devUser uuid.UUID
	log     *slog.Logger
}

func New(cfg config.Config, log *slog.Logger) (*Authenticator, error) {
	a := &Authenticator{secret: []byte(cfg.JWTSecret), devMode: cfg.DevAuthEnable, log: log}
	if a.devMode {
		id, err := uuid.Parse(cfg.DevUserID)
		if err != nil {
			return nil, err
		}
		a.devUser = id
	}
	return a, nil
}

// Middleware requires a valid caller and stores the user id on the request context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := a.resolve(r)
		if err != nil {
			httpapi.Fail(w, r, a.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, userID)))
	})
}

func (a *Authenticator) resolve(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("Authorization")
	token, hasBearer := strings.CutPrefix(header, "Bearer ")

	// Development identity is only consulted when no real token was presented, and
	// config startup already refuses to enable it in production.
	if a.devMode && !hasBearer {
		if override := r.Header.Get("X-Dev-User-Id"); override != "" {
			id, err := uuid.Parse(override)
			if err != nil {
				return uuid.Nil, unauthorized("invalid X-Dev-User-Id")
			}
			return id, nil
		}
		return a.devUser, nil
	}
	if !hasBearer {
		return uuid.Nil, unauthorized("missing bearer token")
	}

	claims := jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(strings.TrimSpace(token), &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return a.secret, nil
	})
	if err != nil || !parsed.Valid {
		return uuid.Nil, unauthorized("invalid token")
	}

	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, unauthorized("token subject is not a user id")
	}
	return id, nil
}

// UserID returns the authenticated caller placed on the context by Middleware.
func UserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKey{}).(uuid.UUID)
	return id, ok
}

func MustUserID(ctx context.Context) uuid.UUID {
	id, _ := UserID(ctx)
	return id
}

func unauthorized(msg string) error {
	return httpapi.NewError(http.StatusUnauthorized, httpapi.CodeUnauthorized, msg)
}
