package pabbly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"course-coupon/internal/money"
	"course-coupon/internal/provider"
)

const pageSize = 100

// ListCatalog reads every product, then every plan of each product, and returns one
// normalized item per sellable Product+Plan pair.
func (c *Client) ListCatalog(ctx context.Context) ([]provider.CatalogItem, error) {
	products, err := c.listProducts(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]provider.CatalogItem, 0, len(products))
	for _, p := range products {
		productID := p.str("_id", "id", "product_id")
		if productID == "" {
			continue
		}
		productName := p.str("product_name", "name", "title")

		var raw json.RawMessage
		if err := c.get(ctx, "/plans/"+url.PathEscape(productID), nil, &raw); err != nil {
			return nil, fmt.Errorf("list plans for product %s: %w", productID, err)
		}
		for _, plan := range collection(raw, "plans", "docs", "data", "result") {
			item, ok := normalizePlan(productID, productName, plan)
			if ok {
				items = append(items, item)
			}
		}
	}
	return items, nil
}

// listProducts walks every page; the first page is not assumed to be complete.
func (c *Client) listProducts(ctx context.Context) ([]object, error) {
	var all []object
	for page := 1; ; page++ {
		q := url.Values{
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(pageSize)},
		}
		var raw json.RawMessage
		if err := c.get(ctx, "/products", q, &raw); err != nil {
			return nil, fmt.Errorf("list products: %w", err)
		}
		batch := collection(raw, "products", "docs", "data", "result")
		all = append(all, batch...)
		if len(batch) < pageSize {
			return all, nil
		}
	}
}

// normalizePlan maps a Pabbly plan onto a marketplace CatalogItem. A plan without a
// usable id or price is skipped rather than failing the whole sync.
func normalizePlan(productID, productName string, plan object) (provider.CatalogItem, bool) {
	planID := plan.str("_id", "id", "plan_id")
	if planID == "" {
		return provider.CatalogItem{}, false
	}

	price, err := money.ParseMajor(plan.raw("price", "plan_price", "amount"))
	if err != nil {
		return provider.CatalogItem{}, false
	}

	meta := plan.nested("meta_data", "metadata", "meta")
	item := provider.CatalogItem{
		Ref:          provider.CourseRef{ProductID: productID, PlanID: planID},
		Title:        productName,
		PlanName:     plan.str("plan_name", "name"),
		Description:  plan.str("plan_description", "description"),
		SellingPrice: price,
		Currency:     currencyOr(plan.str("currency_code", "currency"), "INR"),
		CheckoutURL:  plan.str("checkout_page", "checkout_url", "checkout_page_url"),
		Active:       plan.boolish(true, "plan_active", "active", "status"),

		ProviderCreatedAt: plan.timePtr("created_at", "createdAt"),
		ProviderUpdatedAt: plan.timePtr("updated_at", "updatedAt"),
	}

	if meta != nil {
		// Malformed optional metadata is treated as absent; it must not break the sync.
		if mrp, err := money.ParseMajor(meta.raw("mrp_inr", "mrp")); err == nil && mrp > 0 {
			item.MRP = &mrp
		}
		item.Class = meta.intPtr("class")
		item.Board = meta.str("board")
		item.Subject = meta.str("subject")
		item.Language = meta.str("language")
		item.TargetExam = meta.str("target_exam")
		item.Thumbnail = meta.str("thumbnail_url", "thumbnail")
		item.Duration = meta.str("duration")
	}

	// A provider "discount" that would be negative is not a real discount.
	if item.MRP != nil && *item.MRP < item.SellingPrice {
		item.MRP = nil
	}
	return item, true
}

// GetCommercialState reads the live plan record. This is the authority used before
// any coin or coupon commitment.
func (c *Client) GetCommercialState(ctx context.Context, ref provider.CourseRef) (provider.CommercialState, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/plan/"+url.PathEscape(ref.PlanID), nil, &raw); err != nil {
		return provider.CommercialState{}, err
	}
	plan := single(raw, "plan", "doc", "result")
	if plan == nil {
		return provider.CommercialState{}, fmt.Errorf("pabbly: empty plan response for %s", ref.PlanID)
	}

	item, ok := normalizePlan(ref.ProductID, "", plan)
	if !ok {
		return provider.CommercialState{}, fmt.Errorf("pabbly: plan %s missing price", ref.PlanID)
	}
	return provider.CommercialState{
		SellingPrice: item.SellingPrice,
		MRP:          item.MRP,
		Currency:     item.Currency,
		Active:       item.Active,
		CheckoutURL:  item.CheckoutURL,
		FetchedAt:    time.Now().UTC(),
	}, nil
}

func currencyOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
