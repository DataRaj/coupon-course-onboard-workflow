package pabbly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"course-coupon/internal/provider"
)

// CreateCoupon issues a single-use, plan-scoped percentage coupon.
func (c *Client) CreateCoupon(ctx context.Context, req provider.CouponRequest) (provider.ProviderCoupon, error) {
	body := map[string]any{
		"coupon_name":        req.Name,
		"coupon_code":        req.Code,
		"discount":           bpsToPercentString(req.DiscountBps),
		"discount_type":      "percent",
		"redemption_type":    "onetime",
		"associate_plans":    "selected_plans",
		"plans_array":        []string{req.Ref.PlanID},
		"apply_to":           "subscription_amount",
		"maximum_redemption": 1,
		"valid_upto":         req.ValidUpto.UTC().Format("2006-01-02"),
	}

	var raw json.RawMessage
	if err := c.post(ctx, "/coupon/"+url.PathEscape(req.Ref.ProductID), body, &raw); err != nil {
		return provider.ProviderCoupon{}, err
	}
	coupon := single(raw, "coupon", "doc", "result")
	id := coupon.str("_id", "id", "coupon_id")
	if id == "" {
		return provider.ProviderCoupon{}, fmt.Errorf("pabbly: coupon created without an id")
	}
	return provider.ProviderCoupon{ID: id, Code: firstNonEmpty(coupon.str("coupon_code", "code"), req.Code)}, nil
}

// FindCouponByCode lets the caller resolve an ambiguous create timeout without
// risking a duplicate reward coupon.
func (c *Client) FindCouponByCode(ctx context.Context, productID, code string) (provider.ProviderCoupon, error) {
	for page := 1; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
		var raw json.RawMessage
		if err := c.get(ctx, "/coupon/"+url.PathEscape(productID), q, &raw); err != nil {
			return provider.ProviderCoupon{}, err
		}
		batch := collection(raw, "coupons", "docs", "data", "result")
		for _, item := range batch {
			if strings.EqualFold(item.str("coupon_code", "code"), code) {
				return provider.ProviderCoupon{
					ID:   item.str("_id", "id", "coupon_id"),
					Code: code,
				}, nil
			}
		}
		if len(batch) < pageSize {
			return provider.ProviderCoupon{}, provider.ErrNotFound
		}
	}
}

// DisableCoupon deactivates rather than deletes, so reward history stays intact.
func (c *Client) DisableCoupon(ctx context.Context, productID, couponID string) error {
	body := map[string]any{"status": "inactive"}
	return c.put(ctx, "/coupons/"+url.PathEscape(couponID), body, nil)
}

// GetCheckout resolves the plan's checkout page. Pabbly's coupon query-string field
// is not documented for this account, so we never fabricate one: the caller receives
// the code separately and shows it for manual entry.
func (c *Client) GetCheckout(ctx context.Context, ref provider.CourseRef, couponCode string) (provider.Checkout, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/checkoutpage/"+url.PathEscape(ref.ProductID), nil, &raw); err != nil {
		return provider.Checkout{}, err
	}

	for _, page := range collection(raw, "checkout_pages", "checkoutpages", "docs", "data", "result") {
		if matchesPlan(page, ref.PlanID) {
			if u := page.str("checkout_url", "url", "page_url", "checkout_page"); u != "" {
				return provider.Checkout{URL: u}, nil
			}
		}
	}

	// Fall back to the URL the plan record itself advertises.
	state, err := c.GetCommercialState(ctx, ref)
	if err != nil {
		return provider.Checkout{}, err
	}
	if state.CheckoutURL == "" {
		return provider.Checkout{}, fmt.Errorf("%w: no checkout page for plan %s", provider.ErrNotFound, ref.PlanID)
	}
	return provider.Checkout{URL: state.CheckoutURL}, nil
}

func matchesPlan(page object, planID string) bool {
	if page.str("plan_id", "planId") == planID {
		return true
	}
	if list, ok := page["plans_array"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && s == planID {
				return true
			}
		}
	}
	return false
}

// bpsToPercentString renders basis points as a percentage without float rounding.
func bpsToPercentString(bps int32) string {
	if bps%100 == 0 {
		return strconv.Itoa(int(bps / 100))
	}
	return strings.TrimRight(fmt.Sprintf("%d.%02d", bps/100, bps%100), "0")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
