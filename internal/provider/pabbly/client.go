package pabbly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"course-coupon/internal/config"
	"course-coupon/internal/provider"
)

const maxBodyBytes = 4 << 20

// Client is the low-level Pabbly HTTP transport. It knows about auth, retries and
// error mapping only; normalization lives in the sibling files.
type Client struct {
	base       string
	apiKey     string
	secret     string
	httpClient *http.Client
	log        *slog.Logger
}

func New(cfg config.PabblyConfig, log *slog.Logger) *Client {
	return &Client{
		base:       strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		secret:     cfg.SecretKey,
		httpClient: &http.Client{Timeout: cfg.Timeout},
		log:        log.With("component", "pabbly"),
	}
}

func (c *Client) Code() string { return provider.Pabbly }

func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CatalogWebhook:    false, // No documented plan price-change event; we reconcile instead.
		LivePriceLookup:   true,
		CouponCreate:      true,
		CouponDisable:     true,
		CheckoutLink:      true,
		PurchaseWebhook:   true,
		TransactionLookup: true,
		RefundWebhook:     true,
	}
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("pabbly: status %d: %s", e.Status, truncate(e.Body, 300))
}

// get performs a retryable read. Only safe methods are retried.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out, true)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out, false)
}

func (c *Client) put(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPut, path, nil, body, out, false)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, retry bool) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	endpoint := c.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	attempts := 1
	if retry {
		attempts = 3
	}

	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * 300 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.apiKey, c.secret)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%w: %v", provider.ErrUnavailable, readErr)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", provider.ErrNotFound, path)
		case isTransient(resp.StatusCode):
			lastErr = fmt.Errorf("%w: %s", provider.ErrUnavailable, (&apiError{resp.StatusCode, string(raw)}).Error())
			continue
		case resp.StatusCode >= 300:
			return &apiError{resp.StatusCode, string(raw)}
		}

		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("pabbly: decode %s: %w", path, err)
		}
		return nil
	}
	return lastErr
}

func isTransient(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
