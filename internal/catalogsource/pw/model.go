// Package pw acquires one public Class 12 IIT-JEE batch through Playwright.
// Browser and page-specific types never cross into the course domain.
package pw

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"course-coupon/internal/course"
	"course-coupon/internal/money"
)

const Provider = "PW"
const Version = "pw-extractor-v1"

// PWBatchDTO is the extraction boundary. Empty strings/pointers mean unknown.
// Provenance is deliberately small: field -> network_json/embedded_state/rendered_dom.
type PWBatchDTO struct {
	Provider            string
	ExternalID          string
	Slug                string
	CanonicalURL        string
	Title               string
	Description         string
	Class               *int32
	TargetExam          string
	Series              string
	TargetYear          *int
	Language            string
	Mode                string
	StartDate           *time.Time
	EndDate             *time.Time
	SellingPrice        string
	OriginalPrice       string
	Currency            string
	SellingPriceContext string
	Subjects            []string
	Faculty             []course.Faculty
	Features            []string
	Thumbnail           string
	Availability        string
	AcquiredAt          time.Time
	Warnings            []string
	Provenance          map[string]string
}

type IngestionError struct {
	Code  string
	Cause error
}

func (e *IngestionError) Error() string    { return fmt.Sprintf("pw %s: %v", e.Code, e.Cause) }
func (e *IngestionError) Unwrap() error    { return e.Cause }
func failure(code string, err error) error { return &IngestionError{code, err} }
func errorCode(err error) string {
	var e *IngestionError
	if errors.As(err, &e) {
		return e.Code
	}
	return "ingestion_failed"
}

func targetIdentity(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "www.pw.live" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/iit-jee/class-12/batches/") {
		return "", "", fmt.Errorf("invalid public PW Class 12 target")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	slug := path.Base(u.Path)
	if slug == "batches" || slug == "." || strings.Contains(slug, "%") {
		return "", "", fmt.Errorf("missing batch slug")
	}
	return slug, u.String(), nil
}

// ParsePrice accepts explicit decimal INR amounts only, with conventional Indian
// or international digit grouping. It rejects ranges, EMI text, fractions,
// exponents, overflow and sub-paise precision instead of rounding or guessing.
var pricePattern = regexp.MustCompile(`^(?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+|[0-9]{1,2}(?:,[0-9]{2})*,[0-9]{3})(?:\.[0-9]{1,2})?$`)

func ParsePrice(raw string) (money.Minor, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "₹"), "INR"))
	if !pricePattern.MatchString(s) {
		return 0, fmt.Errorf("invalid price syntax")
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(s, ",", ""))
	if !ok {
		return 0, fmt.Errorf("invalid price")
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || !r.Num().IsInt64() || r.Sign() <= 0 {
		return 0, fmt.Errorf("price must be positive and fit minor units")
	}
	return money.Minor(r.Num().Int64()), nil
}

// Normalize validates identity strictly and commercial fields independently.
// Missing/ambiguous price yields an incomplete observation plus a warning.
func Normalize(d PWBatchDTO, expectedTitle string) (course.CatalogObservation, []string, error) {
	var o course.CatalogObservation
	slug, canonical, err := targetIdentity(d.CanonicalURL)
	if err != nil || d.Provider != Provider || d.Slug != slug || strings.TrimSpace(d.Title) == "" {
		return o, d.Warnings, failure("identity_invalid", fmt.Errorf("provider, target identity and title are required"))
	}
	if expectedTitle != "" && strings.TrimSpace(d.Title) != expectedTitle {
		return o, d.Warnings, failure("title_mismatch", fmt.Errorf("title differs from configured expected title"))
	}
	if d.Class != nil && *d.Class != 12 {
		return o, d.Warnings, failure("audience_mismatch", fmt.Errorf("batch is not Class 12"))
	}
	if d.AcquiredAt.IsZero() {
		return o, d.Warnings, failure("observation_invalid", fmt.Errorf("missing acquisition timestamp"))
	}
	warnings := append([]string{}, d.Warnings...)
	availability := d.Availability
	if availability == "" {
		availability = "unknown"
	}
	o = course.CatalogObservation{Provider: Provider, Slug: slug, ExternalID: d.ExternalID, CanonicalURL: canonical, Title: strings.TrimSpace(d.Title), Description: d.Description, Class: d.Class, Language: d.Language, TargetExam: d.TargetExam, Thumbnail: d.Thumbnail, ObservedAt: d.AcquiredAt,
		Details: course.CatalogDetails{CanonicalURL: canonical, ExternalID: d.ExternalID, Slug: slug, Series: d.Series, TargetYear: d.TargetYear, Mode: d.Mode, Subjects: d.Subjects, Faculty: d.Faculty, Features: d.Features, StartDate: d.StartDate, EndDate: d.EndDate, Availability: availability}}
	price, priceErr := ParsePrice(d.SellingPrice)
	source := d.Provenance["selling_price"]
	if priceErr != nil || d.Currency != "INR" || d.SellingPriceContext != "selling_price" || (source != "network_json" && source != "embedded_state" && source != "rendered_dom") {
		warnings = append(warnings, "commercial_incomplete: no confident positive INR selling price")
		return o, warnings, nil
	}
	o.SellingPrice = &price
	o.Currency = d.Currency
	if d.OriginalPrice != "" {
		mrp, e := ParsePrice(d.OriginalPrice)
		if e == nil && mrp >= price && d.Provenance["original_price"] != "" {
			o.MRP = &mrp
		} else {
			warnings = append(warnings, "original_price_invalid")
		}
	}
	return o, warnings, nil
}

func parseDate(raw string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2 Jan 2006", "02 January 2006", "2 January 2006"} {
		if t, e := time.Parse(layout, strings.TrimSpace(raw)); e == nil {
			return &t
		}
	}
	return nil
}
