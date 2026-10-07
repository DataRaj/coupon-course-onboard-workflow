package course

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"course-coupon/internal/provider"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ApplyCatalog is called in a source-locked transaction. Incomplete observations
// cannot replace any part of the last-known-good projection.
func (s *Store) ApplyCatalog(ctx context.Context, tx pgx.Tx, previousID *uuid.UUID, previousHash string, o CatalogObservation) (uuid.UUID, string, bool, error) {
	if o.Provider == "" || o.Slug == "" || o.Title == "" || o.CanonicalURL == "" {
		return uuid.Nil, "", false, fmt.Errorf("invalid normalized catalog identity")
	}
	if o.SellingPrice == nil {
		if previousID == nil {
			return uuid.Nil, previousHash, false, nil
		}
		_, err := tx.Exec(ctx, `UPDATE courses SET commercial_status='STALE' WHERE id=$1 AND commercial_status IS DISTINCT FROM 'STALE'`, *previousID)
		return *previousID, previousHash, false, err
	}
	if *o.SellingPrice <= 0 || len(o.Currency) != 3 || o.ObservedAt.IsZero() {
		return uuid.Nil, "", false, fmt.Errorf("invalid normalized commercial observation")
	}
	var old Course
	if previousID != nil {
		var err error
		old, err = scanCourse(tx.QueryRow(ctx, `SELECT `+courseColumns+` FROM courses WHERE id=$1 FOR UPDATE`, *previousID))
		if err != nil {
			return uuid.Nil, "", false, err
		}
		// Absence is not evidence of removal. Preserve optional last-known-good data.
		if o.MRP == nil {
			o.MRP = old.MRP
		}
		if o.Description == "" {
			o.Description = old.Description
		}
		if o.Class == nil {
			o.Class = old.Class
		}
		if o.Language == "" {
			o.Language = old.Language
		}
		if o.TargetExam == "" {
			o.TargetExam = old.TargetExam
		}
		if o.Thumbnail == "" {
			o.Thumbnail = old.Thumbnail
		}
		if old.Catalog != nil {
			d := old.Catalog
			if o.Details.ExternalID == "" {
				o.Details.ExternalID = d.ExternalID
			}
			if o.Details.Series == "" {
				o.Details.Series = d.Series
			}
			if o.Details.TargetYear == nil {
				o.Details.TargetYear = d.TargetYear
			}
			if o.Details.Mode == "" {
				o.Details.Mode = d.Mode
			}
			if len(o.Details.Subjects) == 0 {
				o.Details.Subjects = d.Subjects
			}
			if len(o.Details.Faculty) == 0 {
				o.Details.Faculty = d.Faculty
			}
			if len(o.Details.Features) == 0 {
				o.Details.Features = d.Features
			}
			if o.Details.StartDate == nil {
				o.Details.StartDate = d.StartDate
			}
			if o.Details.EndDate == nil {
				o.Details.EndDate = d.EndDate
			}
			if o.Details.Availability == "unknown" {
				o.Details.Availability = d.Availability
			}
		}
	}
	if o.MRP != nil && *o.MRP < *o.SellingPrice {
		o.MRP = nil
	}
	if previousID != nil && o.ExternalID == "" && old.Catalog != nil {
		o.ExternalID = old.Catalog.ExternalID
	}
	hashInput := o
	hashInput.ObservedAt = time.Time{}
	encoded, err := json.Marshal(hashInput)
	if err != nil {
		return uuid.Nil, "", false, err
	}
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	id := old.ID
	changed := previousID == nil || hash != previousHash
	if changed {
		productID := o.ExternalID
		if productID == "" {
			productID = o.Slug
		}
		if previousID != nil {
			productID = old.ProviderProductID
		}
		item := provider.CatalogItem{Ref: provider.CourseRef{ProductID: productID, PlanID: "catalog"}, Title: o.Title, Description: o.Description, Class: o.Class, Language: o.Language, TargetExam: o.TargetExam, Thumbnail: o.Thumbnail, SellingPrice: *o.SellingPrice, MRP: o.MRP, Currency: o.Currency, Active: true}
		if len(o.Details.Subjects) > 0 {
			item.Subject = o.Details.Subjects[0]
		}
		id, _, err = s.Upsert(ctx, tx, o.Provider, item, SourceCatalogIngestion)
		if err != nil {
			return uuid.Nil, "", false, err
		}
		details, e := json.Marshal(o.Details)
		if e != nil {
			return uuid.Nil, "", false, e
		}
		if _, err = tx.Exec(ctx, `UPDATE courses SET catalog_details=$2 WHERE id=$1`, id, details); err != nil {
			return uuid.Nil, "", false, err
		}
	}
	if id == uuid.Nil {
		return id, "", false, errors.New("missing catalog course identity")
	}
	_, err = tx.Exec(ctx, `UPDATE courses SET commercial_observed_at=$2, commercial_status='CURRENT' WHERE id=$1`, id, o.ObservedAt)
	return id, hash, changed, err
}
