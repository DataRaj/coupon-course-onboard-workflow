package course

import (
	"course-coupon/internal/money"
	"time"
)

const SourceCatalogIngestion = "CATALOG_INGESTION"

// CatalogDetails contains only normalized, provider-neutral public attributes.
// Unknown optional attributes remain absent; acquisition internals are not API data.
type CatalogDetails struct {
	CanonicalURL string     `json:"canonical_url"`
	ExternalID   string     `json:"external_id,omitempty"`
	Slug         string     `json:"slug"`
	Series       string     `json:"series,omitempty"`
	TargetYear   *int       `json:"target_year,omitempty"`
	Mode         string     `json:"mode,omitempty"`
	Subjects     []string   `json:"subjects,omitempty"`
	Faculty      []Faculty  `json:"faculty,omitempty"`
	Features     []string   `json:"features,omitempty"`
	StartDate    *time.Time `json:"start_date,omitempty"`
	EndDate      *time.Time `json:"end_date,omitempty"`
	Availability string     `json:"availability"`
}
type Faculty struct {
	Name    string `json:"name"`
	Subject string `json:"subject,omitempty"`
}

// CatalogObservation is accepted only after a source validates its extraction.
// A nil SellingPrice is an incomplete observation, never a zero-price update.
type CatalogObservation struct {
	Provider     string
	Slug         string
	ExternalID   string
	CanonicalURL string
	Title        string
	Description  string
	Class        *int32
	Language     string
	TargetExam   string
	Thumbnail    string
	Details      CatalogDetails
	SellingPrice *money.Minor
	MRP          *money.Minor
	Currency     string
	ObservedAt   time.Time
}
