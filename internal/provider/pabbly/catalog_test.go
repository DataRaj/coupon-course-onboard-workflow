package pabbly

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"course-coupon/internal/config"
	"course-coupon/internal/money"
	"course-coupon/internal/provider"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(config.PabblyConfig{
		BaseURL: srv.URL, APIKey: "key", SecretKey: "secret",
	}, slog.New(slog.DiscardHandler))
}

// One product carrying two active plans must yield two distinct sellable courses.
func TestListCatalogSplitsPlansIntoCourses(t *testing.T) {
	var sawAuth bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		sawAuth = ok && user == "key" && pass == "secret"

		switch {
		case r.URL.Path == "/products":
			if r.URL.Query().Get("page") != "1" {
				writeJSON(w, `{"status":"success","data":{"products":[]}}`)
				return
			}
			writeJSON(w, `{"status":"success","data":{"products":[
				{"_id":"prod_1","product_name":"Class 12 Physics — Board + JEE Foundation"}]}}`)
		case r.URL.Path == "/plans/prod_1":
			writeJSON(w, `{"status":"success","data":{"plans":[
				{"_id":"plan_full","plan_name":"Physics 2026","price":"7000","currency_code":"INR",
				 "plan_description":"Full course","checkout_page":"https://pay.example/full","plan_active":true,
				 "meta_data":{"mrp_inr":"10000","class":"12","board":"CBSE","subject":"Physics",
				              "language":"Hinglish","target_exam":"Boards,JEE"}},
				{"_id":"plan_crash","plan_name":"Physics Crash Course","price":3000,"currency_code":"INR",
				 "plan_active":true}]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	items, err := c.ListCatalog(context.Background())
	if err != nil {
		t.Fatalf("ListCatalog: %v", err)
	}
	if !sawAuth {
		t.Error("expected HTTP basic auth on every request")
	}
	if len(items) != 2 {
		t.Fatalf("want 2 courses from 2 plans, got %d", len(items))
	}

	full := items[0]
	if full.Ref.ProductID != "prod_1" || full.Ref.PlanID != "plan_full" {
		t.Errorf("wrong identity: %+v", full.Ref)
	}
	if full.Title != "Class 12 Physics — Board + JEE Foundation" {
		t.Errorf("title should come from the product: %q", full.Title)
	}
	if full.SellingPrice != money.Minor(700000) {
		t.Errorf("selling price want 700000 paise, got %d", full.SellingPrice)
	}
	if full.MRP == nil || *full.MRP != money.Minor(1000000) {
		t.Errorf("mrp should be parsed from metadata, got %v", full.MRP)
	}
	if full.Class == nil || *full.Class != 12 || full.Board != "CBSE" || full.Language != "Hinglish" {
		t.Errorf("academic metadata not mapped: %+v", full)
	}

	crash := items[1]
	if crash.Ref.PlanID != "plan_crash" || crash.SellingPrice != money.Minor(300000) {
		t.Errorf("second plan wrong: %+v", crash)
	}
	if crash.MRP != nil {
		t.Error("absent MRP must not be invented")
	}
}

// Malformed optional metadata is treated as missing rather than failing the sync.
func TestNormalizePlanTolerantMetadata(t *testing.T) {
	plan := object{
		"_id": "plan_x", "plan_name": "P", "price": "1500",
		"meta_data": `{"mrp_inr":"not-a-number","class":"Class 11"}`,
	}
	item, ok := normalizePlan("prod_x", "Product", plan)
	if !ok {
		t.Fatal("plan with a valid price should normalize")
	}
	if item.MRP != nil {
		t.Errorf("unparseable mrp must be dropped, got %v", item.MRP)
	}
	if item.Class == nil || *item.Class != 11 {
		t.Errorf("class should tolerate a 'Class 11' string, got %v", item.Class)
	}
}

// An MRP below the selling price is not a real provider discount.
func TestNormalizePlanRejectsFakeDiscount(t *testing.T) {
	item, _ := normalizePlan("p", "P", object{
		"_id": "plan_y", "price": "7000", "meta_data": map[string]any{"mrp_inr": "5000"},
	})
	if item.MRP != nil {
		t.Errorf("mrp below selling price must be suppressed, got %v", item.MRP)
	}
}

// Catalog listing must not stop at the first page.
func TestListProductsPaginates(t *testing.T) {
	var pages []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/plans/") {
			writeJSON(w, `{"data":{"plans":[]}}`)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if page == "1" {
			var b strings.Builder
			b.WriteString(`{"data":{"products":[`)
			for i := range pageSize {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"_id":"p`)
				b.WriteString(strconv.Itoa(i))
				b.WriteString(`","product_name":"x"}`)
			}
			b.WriteString(`]}}`)
			writeJSON(w, b.String())
			return
		}
		writeJSON(w, `{"data":{"products":[{"_id":"last","product_name":"y"}]}}`)
	})

	if _, err := c.ListCatalog(context.Background()); err != nil {
		t.Fatalf("ListCatalog: %v", err)
	}
	if len(pages) < 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("expected pagination beyond page 1, saw %v", pages)
	}
}

// The live plan read is what claim and checkout trust.
func TestGetCommercialState(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plan/plan_full" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeJSON(w, `{"status":"success","data":{"plan":{"_id":"plan_full","price":"8000",
			"currency_code":"INR","plan_active":true,"checkout_page":"https://pay.example/full"}}}`)
	})

	state, err := c.GetCommercialState(context.Background(),
		provider.CourseRef{ProductID: "prod_1", PlanID: "plan_full"})
	if err != nil {
		t.Fatalf("GetCommercialState: %v", err)
	}
	if state.SellingPrice != money.Minor(800000) {
		t.Errorf("want 800000 paise, got %d", state.SellingPrice)
	}
	if !state.Active || state.CheckoutURL == "" {
		t.Errorf("unexpected state %+v", state)
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}
