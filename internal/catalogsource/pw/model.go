// Package pw discovers and extracts public PW batch pages through Playwright.
// Browser and page-specific types never leave this package.
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

	"course-coupon/internal/money"
)

const Provider = "PW"
const Version = "pw-extractor-v2"

// Target is the immutable input for one detail-page acquisition.
type Target struct {
	Slug         string `json:"slug"`
	CanonicalURL string `json:"canonical_url"`
	Title        string `json:"title,omitempty"`
}

type Faculty struct {
	Name       string `json:"name"`
	Subject    string `json:"subject,omitempty"`
	Experience string `json:"experience,omitempty"`
}

type PWPlanDTO struct {
	Name          string   `json:"name"`
	SellingPrice  string   `json:"selling_price,omitempty"`
	OriginalPrice string   `json:"original_price,omitempty"`
	Features      []string `json:"features,omitempty"`
}

type BatchPlan struct {
	Name               string       `json:"name"`
	SellingPriceMinor  *money.Minor `json:"selling_price_minor,omitempty"`
	OriginalPriceMinor *money.Minor `json:"original_price_minor,omitempty"`
	Features           []string     `json:"features,omitempty"`
}

// PWBatchDTO is the provider-specific extraction boundary. Empty values mean
// unknown; values are never invented from a title.
type PWBatchDTO struct {
	Provider            string            `json:"provider"`
	ExternalID          string            `json:"external_id,omitempty"`
	Slug                string            `json:"slug"`
	CanonicalURL        string            `json:"canonical_url"`
	Title               string            `json:"title"`
	Description         string            `json:"description,omitempty"`
	Class               *int32            `json:"class,omitempty"`
	TargetExam          string            `json:"target_exam,omitempty"`
	Series              string            `json:"series,omitempty"`
	TargetYear          *int              `json:"target_year,omitempty"`
	Language            string            `json:"language,omitempty"`
	Mode                string            `json:"mode,omitempty"`
	StartDate           *time.Time        `json:"start_date,omitempty"`
	EndDate             *time.Time        `json:"end_date,omitempty"`
	SellingPrice        string            `json:"selling_price,omitempty"`
	OriginalPrice       string            `json:"original_price,omitempty"`
	Discount            string            `json:"discount,omitempty"`
	Currency            string            `json:"currency,omitempty"`
	SellingPriceContext string            `json:"-"`
	Subjects            []string          `json:"subjects,omitempty"`
	Faculty             []Faculty         `json:"faculty,omitempty"`
	Features            []string          `json:"features,omitempty"`
	Schedule            string            `json:"schedule,omitempty"`
	Plans               []PWPlanDTO       `json:"plans,omitempty"`
	Thumbnail           string            `json:"thumbnail_url,omitempty"`
	Availability        string            `json:"availability"`
	AcquiredAt          time.Time         `json:"acquired_at"`
	Warnings            []string          `json:"warnings,omitempty"`
	Provenance          map[string]string `json:"provenance,omitempty"`
}

// NormalizedBatch is safe output for callers of the bulk scraper. Prices use
// integer minor units and remain nil when their meaning cannot be validated.
type NormalizedBatch struct {
	Provider           string            `json:"provider"`
	ExternalID         string            `json:"external_id,omitempty"`
	Slug               string            `json:"slug"`
	CanonicalURL       string            `json:"canonical_url"`
	Title              string            `json:"title"`
	Description        string            `json:"description,omitempty"`
	Class              *int32            `json:"class,omitempty"`
	TargetExam         string            `json:"target_exam,omitempty"`
	Series             string            `json:"series,omitempty"`
	TargetYear         *int              `json:"target_year,omitempty"`
	Language           string            `json:"language,omitempty"`
	Mode               string            `json:"mode,omitempty"`
	StartDate          *time.Time        `json:"start_date,omitempty"`
	EndDate            *time.Time        `json:"end_date,omitempty"`
	SellingPriceMinor  *money.Minor      `json:"selling_price_minor,omitempty"`
	OriginalPriceMinor *money.Minor      `json:"original_price_minor,omitempty"`
	Discount           string            `json:"discount,omitempty"`
	Currency           string            `json:"currency,omitempty"`
	Subjects           []string          `json:"subjects,omitempty"`
	Faculty            []Faculty         `json:"faculty,omitempty"`
	Features           []string          `json:"features,omitempty"`
	Schedule           string            `json:"schedule,omitempty"`
	Plans              []BatchPlan       `json:"plans,omitempty"`
	Thumbnail          string            `json:"thumbnail_url,omitempty"`
	Availability       string            `json:"availability"`
	AcquiredAt         time.Time         `json:"acquired_at"`
	Provenance         map[string]string `json:"provenance,omitempty"`
}

type ScrapeError struct {
	Code  string
	Cause error
}

func (e *ScrapeError) Error() string       { return fmt.Sprintf("pw %s: %v", e.Code, e.Cause) }
func (e *ScrapeError) Unwrap() error       { return e.Cause }
func failure(code string, err error) error { return &ScrapeError{Code: code, Cause: err} }
func errorCode(err error) string {
	var e *ScrapeError
	if errors.As(err, &e) {
		return e.Code
	}
	return "scrape_failed"
}

func targetIdentity(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "www.pw.live" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("invalid public PW target")
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	marker := "/batches/"
	index := strings.LastIndex(cleanPath, marker)
	if index < 0 || strings.HasPrefix(cleanPath, "/study-v2/") || strings.Contains(cleanPath, "/user/") || strings.Contains(cleanPath, "/purchase/") {
		return "", "", fmt.Errorf("target is not a public batch detail URL")
	}
	slug := cleanPath[index+len(marker):]
	if slug == "" || strings.Contains(slug, "/") || path.Base(cleanPath) != slug || strings.ContainsAny(slug, "%?#") {
		return "", "", fmt.Errorf("missing or invalid batch slug")
	}
	u.Path = cleanPath
	return slug, u.String(), nil
}

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
// An ambiguous price produces a usable batch with a warning and no price.
func Normalize(d PWBatchDTO) (NormalizedBatch, []string, error) {
	var n NormalizedBatch
	slug, canonical, err := targetIdentity(d.CanonicalURL)
	if err != nil || d.Provider != Provider || d.Slug != slug || strings.TrimSpace(d.Title) == "" || d.AcquiredAt.IsZero() {
		return n, d.Warnings, failure("identity_invalid", fmt.Errorf("provider, target identity, title and acquisition time are required"))
	}
	availability := d.Availability
	if availability == "" {
		availability = "unknown"
	}
	n = NormalizedBatch{
		Provider: Provider, ExternalID: d.ExternalID, Slug: slug,
		CanonicalURL: canonical, Title: strings.TrimSpace(d.Title),
		Description: d.Description, Class: d.Class, TargetExam: d.TargetExam,
		Series: d.Series, TargetYear: d.TargetYear, Language: d.Language,
		Mode: d.Mode, StartDate: d.StartDate, EndDate: d.EndDate,
		Subjects: d.Subjects, Faculty: d.Faculty, Features: d.Features,
		Schedule: d.Schedule, Discount: d.Discount,
		Thumbnail: d.Thumbnail, Availability: availability,
		AcquiredAt: d.AcquiredAt, Provenance: d.Provenance,
	}
	warnings := append([]string{}, d.Warnings...)
	for _, sourcePlan := range d.Plans {
		plan := BatchPlan{Name: strings.TrimSpace(sourcePlan.Name), Features: sourcePlan.Features}
		if plan.Name == "" {
			continue
		}
		if value, parseErr := ParsePrice(sourcePlan.SellingPrice); parseErr == nil && d.Currency == "INR" {
			plan.SellingPriceMinor = &value
			if original, originalErr := ParsePrice(sourcePlan.OriginalPrice); originalErr == nil && original >= value {
				plan.OriginalPriceMinor = &original
			}
		}
		n.Plans = append(n.Plans, plan)
	}
	price, priceErr := ParsePrice(d.SellingPrice)
	source := d.Provenance["selling_price"]
	if priceErr != nil || d.Currency != "INR" || d.SellingPriceContext != "selling_price" || (source != "network_json" && source != "embedded_state" && source != "rendered_dom") {
		warnings = append(warnings, "commercial_incomplete: no confident positive INR selling price")
		return n, warnings, nil
	}
	n.SellingPriceMinor = &price
	n.Currency = d.Currency
	if d.OriginalPrice != "" {
		mrp, parseErr := ParsePrice(d.OriginalPrice)
		if parseErr == nil && mrp >= price && d.Provenance["original_price"] != "" {
			n.OriginalPriceMinor = &mrp
		} else {
			warnings = append(warnings, "original_price_invalid")
		}
	}
	return n, warnings, nil
}

func parseDate(raw string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2 Jan 2006", "02 January 2006", "2 January 2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return &t
		}
	}
	return nil
}
