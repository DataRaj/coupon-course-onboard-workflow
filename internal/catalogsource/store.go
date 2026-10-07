// Package catalogsource records catalog acquisition independently of commerce.
package catalogsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool    *pgxpool.Pool
	Courses *course.Store
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool, course.NewStore(pool)} }

type Run struct{ ID, SourceID uuid.UUID }
type Diagnostics struct {
	AcquiredAt *time.Time
	Provenance map[string]string
	Warnings   []string
	ErrorCode  string
	Error      string
}

// Start is committed before browser startup so runtime failures remain observable.
// The caller holds a provider/target advisory lock until Finish completes.
func (s *Store) Start(ctx context.Context, provider, slug, canonicalURL, version string) (Run, error) {
	r := Run{ID: uuid.New()}
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		// Recover an abandoned run after process death. The target lock ensures it is
		// not an active attempt. No course projection is changed here.
		err := tx.QueryRow(ctx, `INSERT INTO catalog_sources
   (id,provider,slug,canonical_url,last_attempted_at,extractor_version,result)
   VALUES ($1,$2,$3,$4,now(),$5,'RUNNING')
   ON CONFLICT(provider,slug) DO UPDATE SET last_attempted_at=now(),extractor_version=$5,result='RUNNING'
   RETURNING id`, uuid.New(), provider, slug, canonicalURL, version).Scan(&r.SourceID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE catalog_ingestion_runs SET result='FAILED',finished_at=now(),error_code='interrupted',error='previous process ended before completing ingestion' WHERE source_id=$1 AND result='RUNNING'`, r.SourceID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO catalog_ingestion_runs(id,source_id,extractor_version,result) VALUES($1,$2,$3,'RUNNING')`, r.ID, r.SourceID, version)
		return err
	})
	return r, err
}

// Finish atomically commits projection, snapshot, freshness and diagnostics.
// A nil observation records failure only, never disappearance or deactivation.
func (s *Store) Finish(ctx context.Context, r Run, o *course.CatalogObservation, d Diagnostics) (uuid.UUID, bool, error) {
	var id uuid.UUID
	var changed bool
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var previousID *uuid.UUID
		var hash *string
		var externalID *string
		var provider, slug, canonical string
		if err := tx.QueryRow(ctx, `SELECT course_id,normalized_hash,provider,slug,canonical_url,external_id FROM catalog_sources WHERE id=$1 FOR UPDATE`, r.SourceID).Scan(&previousID, &hash, &provider, &slug, &canonical, &externalID); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT result='RUNNING' FROM catalog_ingestion_runs WHERE id=$1 AND source_id=$2 FOR UPDATE`, r.ID, r.SourceID).Scan(&running); err != nil {
			return err
		}
		if !running {
			return errors.New("ingestion run already finished")
		}
		result := "FAILED"
		normalizedHash := ""
		if hash != nil {
			normalizedHash = *hash
		}
		if previousID != nil {
			id = *previousID
		}
		if o != nil {
			if o.Provider != provider || o.Slug != slug || o.CanonicalURL != canonical {
				return errors.New("catalog observation does not match run target")
			}
			if externalID != nil && o.ExternalID != "" && *externalID != o.ExternalID {
				return errors.New("public external identity changed for target slug")
			}
			var err error
			id, normalizedHash, changed, err = s.Courses.ApplyCatalog(ctx, tx, previousID, normalizedHash, *o)
			if err != nil {
				return err
			}
			result = "INCOMPLETE"
			if o.SellingPrice != nil {
				result = "SUCCEEDED"
			}
			if o.ExternalID != "" {
				if _, err = tx.Exec(ctx, `UPDATE catalog_sources SET external_id=COALESCE(external_id,$2) WHERE id=$1 AND (external_id IS NULL OR external_id=$2)`, r.SourceID, o.ExternalID); err != nil {
					return err
				}
			}
		} else if previousID != nil {
			if _, err := tx.Exec(ctx, `UPDATE courses SET commercial_status='STALE' WHERE id=$1 AND commercial_status IS DISTINCT FROM 'STALE'`, id); err != nil {
				return err
			}
		}
		var courseID *uuid.UUID
		if id != uuid.Nil {
			courseID = &id
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_sources SET course_id=$2,normalized_hash=NULLIF($3,''),result=$4,
   last_successful_at=CASE WHEN $4='SUCCEEDED' THEN now() ELSE last_successful_at END WHERE id=$1`, r.SourceID, courseID, normalizedHash, result); err != nil {
			return err
		}
		if d.Warnings == nil {
			d.Warnings = []string{}
		}
		if d.Provenance == nil {
			d.Provenance = map[string]string{}
		}
		warnings, err := json.Marshal(d.Warnings)
		if err != nil {
			return err
		}
		provenance, err := json.Marshal(d.Provenance)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE catalog_ingestion_runs SET finished_at=now(),result=$2,acquired_at=$3,
   normalized_hash=NULLIF($4,''),provenance=$5,warnings=$6,error_code=NULLIF($7,''),error=NULLIF($8,'') WHERE id=$1`, r.ID, result, d.AcquiredAt, normalizedHash, provenance, warnings, d.ErrorCode, d.Error)
		return err
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("finish ingestion: %w", err)
	}
	return id, changed, nil
}
