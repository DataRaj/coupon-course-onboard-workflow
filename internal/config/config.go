package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env         string
	HTTPAddr    string
	DatabaseURL string

	JWTSecret     string
	DevAuthEnable bool
	DevUserID     string

	Pabbly PabblyConfig

	CatalogSyncInterval   time.Duration
	CatalogHardStaleAfter time.Duration
	PriceQuoteMaxAge      time.Duration
	RedemptionTTL         time.Duration

	WebhookSecret string
}

type PabblyConfig struct {
	BaseURL   string
	APIKey    string
	SecretKey string
	Timeout   time.Duration
}

func Load() (Config, error) {
	c := Config{
		Env:         env("APP_ENV", "development"),
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", ""),

		JWTSecret:     env("JWT_SECRET", ""),
		DevAuthEnable: env("DEV_AUTH_ENABLED", "false") == "true",
		DevUserID:     env("DEV_USER_ID", "00000000-0000-0000-0000-0000000000de"),

		Pabbly: PabblyConfig{
			BaseURL:   env("PABBLY_BASE_URL", "https://payments.pabbly.com/api/v1"),
			APIKey:    env("PABBLY_API_KEY", ""),
			SecretKey: env("PABBLY_SECRET_KEY", ""),
			Timeout:   dur("PABBLY_TIMEOUT", 15*time.Second),
		},

		CatalogSyncInterval:   dur("CATALOG_SYNC_INTERVAL", 2*time.Minute),
		CatalogHardStaleAfter: dur("CATALOG_HARD_STALE_AFTER", 30*time.Minute),
		PriceQuoteMaxAge:      dur("PRICE_QUOTE_MAX_AGE", 60*time.Second),
		RedemptionTTL:         dur("REDEMPTION_TTL", 30*time.Minute),

		WebhookSecret: env("PABBLY_WEBHOOK_SECRET", ""),
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	// Development identity bypasses the real token check; it must never reach production.
	if c.DevAuthEnable && c.Env == "production" {
		return c, fmt.Errorf("DEV_AUTH_ENABLED must not be set in production")
	}
	if !c.DevAuthEnable && c.JWTSecret == "" {
		return c, fmt.Errorf("JWT_SECRET is required unless DEV_AUTH_ENABLED=true")
	}
	if c.WebhookSecret == "" {
		return c, fmt.Errorf("PABBLY_WEBHOOK_SECRET is required")
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
