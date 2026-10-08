package pw

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const fixtureURL = "https://www.pw.live/iit-jee/class-12/batches/lakshya-jee-in-english-2027-348091"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func fixtureDOM(t *testing.T) DOMState {
	t.Helper()
	var d DOMState
	if e := json.Unmarshal(fixture(t, "dom.json"), &d); e != nil {
		t.Fatal(e)
	}
	return d
}
func TestExtractionSources(t *testing.T) {
	for _, method := range []string{"network_json", "embedded_state", "flight", "rendered_dom"} {
		t.Run(method, func(t *testing.T) {
			var network []Payload
			var embedded []string
			var flight string
			switch method {
			case "network_json":
				network = []Payload{{Body: fixture(t, "batch.json")}}
			case "embedded_state":
				embedded = []string{string(fixture(t, "batch.json"))}
			case "flight":
				flight = string(fixture(t, "flight.txt"))
			}
			d, e := Extract(fixtureURL, time.Now(), network, embedded, flight, fixtureDOM(t))
			if e != nil {
				t.Fatal(e)
			}
			o, w, e := Normalize(d)
			if e != nil {
				t.Fatal(e)
			}
			want := int64(389920)
			source := method
			if method == "flight" {
				source = "embedded_state"
			}
			if method == "rendered_dom" {
				want = 389900
			}
			if o.SellingPriceMinor == nil || int64(*o.SellingPriceMinor) != want {
				t.Fatalf("price=%v warnings=%v", o.SellingPriceMinor, w)
			}
			if d.Provenance["selling_price"] != source {
				t.Fatalf("provenance=%v", d.Provenance)
			}
			if o.OriginalPriceMinor == nil || *o.OriginalPriceMinor != 630000 || o.Language != "English" || len(o.Subjects) != 3 || len(o.Faculty) != 4 || len(o.Features) != 7 || o.Thumbnail == "" || len(o.Plans) == 0 {
				t.Fatalf("bad normalized course: %+v", o)
			}
			if o.Faculty[0].Subject != "Physics" || o.StartDate == nil || o.EndDate == nil {
				t.Fatalf("missing relationships/dates: %+v", o)
			}
		})
	}
}
func TestPriceValidation(t *testing.T) {
	for _, s := range []string{"0", "-1", "", "₹4,999 / month", "1/2", "1e4", "12,34", "9.999", "9999999999999999999999999", "₹4,999–5,999", "NaN"} {
		if _, e := ParsePrice(s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for s, want := range map[string]int64{"₹4,999": 499900, "3899.2": 389920, "INR 1,00,000": 10000000, "1234.50": 123450} {
		v, e := ParsePrice(s)
		if e != nil || int64(v) != want {
			t.Errorf("%s: %v %v", s, v, e)
		}
	}
}
func TestIncompleteAndChangedPage(t *testing.T) {
	cases := []struct {
		name      string
		dom       DOMState
		wantErr   bool
		wantPrice bool
	}{
		{"missing_required", DOMState{}, true, false},
		{"changed_structure", DOMState{Title: "Lakshya JEE in English 2027"}, false, false},
		{"unlabelled_price", DOMState{Title: "Lakshya JEE in English 2027", BasePlan: "₹100\n₹200"}, false, false},
		{"only_infinity", DOMState{Title: "Lakshya JEE in English 2027", BasePlan: "Infinity\n₹5199\nSelect"}, false, false},
		{"missing_optional", DOMState{Title: "Lakshya JEE in English 2027", BasePlan: "Batch\n₹3899\nSelect"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, e := Extract(fixtureURL, time.Now(), nil, nil, "", tc.dom)
			if (e != nil) != tc.wantErr {
				t.Fatalf("error=%v", e)
			}
			if e != nil {
				return
			}
			o, _, e := Normalize(d)
			if e != nil {
				t.Fatal(e)
			}
			if (o.SellingPriceMinor != nil) != tc.wantPrice {
				t.Fatalf("price=%v", o.SellingPriceMinor)
			}
			if o.OriginalPriceMinor != nil {
				t.Fatal("invented MRP")
			}
		})
	}
}
func TestConflictingPricesAndSEO(t *testing.T) {
	dom := fixtureDOM(t)
	dom.BasePlan = strings.ReplaceAll(dom.BasePlan, "₹3,899", "₹4,999")
	d, e := Extract(fixtureURL, time.Now(), nil, []string{string(fixture(t, "batch.json"))}, "", dom)
	if e != nil {
		t.Fatal(e)
	}
	o, _, e := Normalize(d)
	if e != nil || o.SellingPriceMinor != nil {
		t.Fatalf("conflicting price accepted: %v %v", o.SellingPriceMinor, e)
	}
	seo := `{"@type":"Product","name":"Lakshya JEE in English 2027","offers":{"price":5200,"priceCurrency":"INR"}}`
	d, e = Extract(fixtureURL, time.Now(), nil, []string{seo}, "", DOMState{Title: "Lakshya JEE in English 2027"})
	if e != nil {
		t.Fatal(e)
	}
	o, _, _ = Normalize(d)
	if o.SellingPriceMinor != nil {
		t.Fatal("SEO offer treated as current price")
	}
}
func TestIdentityAndCommercialValidation(t *testing.T) {
	d, e := Extract(fixtureURL, time.Now(), nil, []string{string(fixture(t, "batch.json"))}, "", fixtureDOM(t))
	if e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*PWBatchDTO){func(d *PWBatchDTO) { d.Currency = "XYZ" }, func(d *PWBatchDTO) { d.SellingPriceContext = "emi" }, func(d *PWBatchDTO) { d.SellingPrice = "broken" }, func(d *PWBatchDTO) { d.Provenance = map[string]string{} }} {
		copy := d
		mutate(&copy)
		o, _, e := Normalize(copy)
		if e != nil || o.SellingPriceMinor != nil {
			t.Fatalf("commercial guard failed: %v", e)
		}
	}
	d.Slug = "wrong"
	if _, _, e = Normalize(d); e == nil {
		t.Fatal("expected identity mismatch")
	}
	for _, u := range []string{"http://www.pw.live/iit-jee/class-12/batches/x", "https://evil.test/iit-jee/class-12/batches/x", "https://www.pw.live/study-v2/batches/x", fixtureURL + "?token=x", fixtureURL + "/nested"} {
		if _, _, e := targetIdentity(u); e == nil {
			t.Errorf("accepted URL %s", u)
		}
	}
}

func TestDiscoverTargets(t *testing.T) {
	listing := "https://www.pw.live/iit-jee/class-12/batches"
	payload := Payload{Body: []byte(`{"data":[{"_id":"1","slug":"alpha","name":"Alpha"},{"href":"/iit-jee/class-12/batches/beta","title":"Beta"}]}`)}
	targets := DiscoverTargets(listing, []Payload{payload}, nil, "", []string{
		"/iit-jee/class-12/batches/alpha",
		"https://evil.test/iit-jee/class-12/batches/evil",
	}, 10, nil)
	if len(targets) != 2 || targets[0].Slug != "alpha" || targets[1].Slug != "beta" {
		t.Fatalf("unexpected targets: %+v", targets)
	}
	selected := map[string]struct{}{"beta": {}}
	targets = DiscoverTargets(listing, []Payload{payload}, nil, "", nil, 10, selected)
	if len(targets) != 1 || targets[0].Slug != "beta" {
		t.Fatalf("slug filter failed: %+v", targets)
	}
	broad := "https://www.pw.live/iit-jee/batches"
	targets = DiscoverTargets(broad, nil, nil, "", []string{"/iit-jee/class-11/batches/arjuna-jee-2027-123456"}, 10, nil)
	if len(targets) != 1 || targets[0].Slug != "arjuna-jee-2027-123456" {
		t.Fatalf("broad catalog target failed: %+v", targets)
	}
	listings := childListingURLs(broad, []string{
		"/iit-jee/class-11/batches", "/iit-jee/class-12/batches/one", "/neet/class-11/batches",
		"https://evil.test/iit-jee/class-12/batches", "/iit-jee/dropper/batches",
	})
	if len(listings) != 2 || listings[0] != "https://www.pw.live/iit-jee/class-11/batches" || listings[1] != "https://www.pw.live/iit-jee/dropper/batches" {
		t.Fatalf("child listing discovery failed: %+v", listings)
	}
	listings = appendMissingURLs(listings, publicIITJEEListings(broad))
	if len(listings) != 3 || listings[2] != "https://www.pw.live/iit-jee/class-12/batches" {
		t.Fatalf("missing public category fallback: %+v", listings)
	}
}
func TestDatesAndMalformedStructuredData(t *testing.T) {
	if parseDate("2026-99-99") != nil || parseDate("20 Apr 2026") == nil {
		t.Fatal("date parsing")
	}
	d, e := Extract(fixtureURL, time.Now(), []Payload{{Body: []byte(`{"data":`)}}, nil, "broken-flight", fixtureDOM(t))
	if e != nil || d.Provenance["selling_price"] != "rendered_dom" {
		t.Fatalf("fallback failed: %v", e)
	}
	if len(flightObjects("1:Tffff,short")) != 0 {
		t.Fatal("accepted truncated text record")
	}
}
