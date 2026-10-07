package course

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"course-coupon/internal/money"
	"course-coupon/internal/provider"
)

var ErrNotFound = errors.New("course not found")

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const courseColumns = `id, provider, provider_product_id, provider_plan_id, title, plan_name,
	description, class, board, subject, language, target_exam, thumbnail_url, duration,
	mrp_amount, selling_price_amount, currency, checkout_url, provider_active,
	last_synced_at, created_at, updated_at, catalog_details, commercial_observed_at, commercial_status`

func scanCourse(row pgx.Row) (Course, error) {
	var c Course
	var details []byte
	var commercialStatus *string
	var board, subject, language, targetExam, thumb, duration *string
	var mrp *int64
	var selling int64
	err := row.Scan(&c.ID, &c.Provider, &c.ProviderProductID, &c.ProviderPlanID, &c.Title,
		&c.PlanName, &c.Description, &c.Class, &board, &subject, &language, &targetExam,
		&thumb, &duration, &mrp, &selling, &c.Currency, &c.CheckoutURL, &c.ProviderActive,
		&c.LastSyncedAt, &c.CreatedAt, &c.UpdatedAt, &details, &c.CommercialObservedAt, &commercialStatus)
	if err != nil {
		return c, err
	}
	if len(details) > 0 {
		if err := json.Unmarshal(details, &c.Catalog); err != nil {
			return c, err
		}
	}
	c.CommercialStatus = deref(commercialStatus)
	c.Board, c.Subject, c.Language = deref(board), deref(subject), deref(language)
	c.TargetExam, c.Thumbnail, c.Duration = deref(targetExam), deref(thumb), deref(duration)
	c.SellingPrice = money.Minor(selling)
	if mrp != nil {
		m := money.Minor(*mrp)
		c.MRP = &m
	}
	return c, nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Course, error) {
	c, err := scanCourse(s.pool.QueryRow(ctx,
		`SELECT `+courseColumns+` FROM courses WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// List returns active courses matching the filter, plus the total match count.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Course, int, error) {
	where := []string{"provider_active"}
	var args []any
	add := func(template string, val any) {
		args = append(args, val)
		where = append(where, strings.ReplaceAll(template, "?", fmt.Sprintf("$%d", len(args))))
	}

	if f.Class != nil {
		add("class = ?", *f.Class)
	}
	if f.Board != "" {
		add("board = ?", f.Board)
	}
	if f.Subject != "" {
		add("subject = ?", f.Subject)
	}
	if f.Language != "" {
		add("language = ?", f.Language)
	}
	if f.TargetExam != "" {
		add("target_exam ILIKE '%' || ? || '%'", f.TargetExam)
	}
	if f.Search != "" {
		add("(title ILIKE '%' || ? || '%' OR plan_name ILIKE '%' || ? || '%')", f.Search)
	}
	clause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM courses `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, f.Limit, f.Offset())
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM courses %s ORDER BY title, plan_name LIMIT $%d OFFSET $%d`,
		courseColumns, clause, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Course
	for rows.Next() {
		c, err := scanCourse(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ActiveOffer returns the live platform offer for a course, if any.
func (s *Store) ActiveOffer(ctx context.Context, courseID uuid.UUID) (Offer, bool, error) {
	var o Offer
	err := s.pool.QueryRow(ctx, `SELECT id, course_id, discount_bps, coin_cost, starts_at, ends_at, status
		FROM offers
		WHERE course_id = $1 AND status = 'ACTIVE'
		  AND starts_at <= now() AND (ends_at IS NULL OR ends_at > now())`, courseID).
		Scan(&o.ID, &o.CourseID, &o.DiscountBps, &o.CoinCost, &o.StartsAt, &o.EndsAt, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, false, nil
	}
	return o, err == nil, err
}

// ActiveOffers bulk-loads offers for a listing page.
func (s *Store) ActiveOffers(ctx context.Context, courseIDs []uuid.UUID) (map[uuid.UUID]Offer, error) {
	out := map[uuid.UUID]Offer{}
	if len(courseIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, course_id, discount_bps, coin_cost, starts_at, ends_at, status
		FROM offers
		WHERE course_id = ANY($1) AND status = 'ACTIVE'
		  AND starts_at <= now() AND (ends_at IS NULL OR ends_at > now())`, courseIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o Offer
		if err := rows.Scan(&o.ID, &o.CourseID, &o.DiscountBps, &o.CoinCost, &o.StartsAt, &o.EndsAt, &o.Status); err != nil {
			return nil, err
		}
		out[o.CourseID] = o
	}
	return out, rows.Err()
}

// Upsert writes the provider projection and appends a price snapshot only when a
// commercially relevant value actually changed. Historical snapshots are immutable.
func (s *Store) Upsert(ctx context.Context, tx pgx.Tx, providerCode string, item provider.CatalogItem, source string) (uuid.UUID, bool, error) {
	var (
		id       uuid.UUID
		prevMRP  *int64
		prevSell *int64
		prevCur  string
		found    bool
	)
	err := tx.QueryRow(ctx, `SELECT id, mrp_amount, selling_price_amount, currency FROM courses
		WHERE provider = $1 AND provider_product_id = $2 AND provider_plan_id = $3 FOR UPDATE`,
		providerCode, item.Ref.ProductID, item.Ref.PlanID).Scan(&id, &prevMRP, &prevSell, &prevCur)
	switch {
	case err == nil:
		found = true
	case errors.Is(err, pgx.ErrNoRows):
		id = uuid.New()
	default:
		return uuid.Nil, false, err
	}

	var mrp *int64
	if item.MRP != nil {
		v := int64(*item.MRP)
		mrp = &v
	}

	if _, err := tx.Exec(ctx, `INSERT INTO courses (
			id, provider, provider_product_id, provider_plan_id, title, plan_name, description,
			class, board, subject, language, target_exam, thumbnail_url, duration,
			mrp_amount, selling_price_amount, currency, checkout_url, provider_active,
			provider_created_at, provider_updated_at, last_synced_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21, now())
		ON CONFLICT (provider, provider_product_id, provider_plan_id) DO UPDATE SET
			title = EXCLUDED.title, plan_name = EXCLUDED.plan_name,
			description = EXCLUDED.description, class = EXCLUDED.class, board = EXCLUDED.board,
			subject = EXCLUDED.subject, language = EXCLUDED.language,
			target_exam = EXCLUDED.target_exam, thumbnail_url = EXCLUDED.thumbnail_url,
			duration = EXCLUDED.duration, mrp_amount = EXCLUDED.mrp_amount,
			selling_price_amount = EXCLUDED.selling_price_amount, currency = EXCLUDED.currency,
			checkout_url = EXCLUDED.checkout_url, provider_active = EXCLUDED.provider_active,
			provider_created_at = COALESCE(EXCLUDED.provider_created_at, courses.provider_created_at),
			provider_updated_at = COALESCE(EXCLUDED.provider_updated_at, courses.provider_updated_at),
			last_synced_at = now(), updated_at = now()`,
		id, providerCode, item.Ref.ProductID, item.Ref.PlanID, item.Title, item.PlanName,
		item.Description, item.Class, nilIfEmpty(item.Board), nilIfEmpty(item.Subject),
		nilIfEmpty(item.Language), nilIfEmpty(item.TargetExam), nilIfEmpty(item.Thumbnail),
		nilIfEmpty(item.Duration), mrp, int64(item.SellingPrice), item.Currency,
		item.CheckoutURL, item.Active, item.ProviderCreatedAt, item.ProviderUpdatedAt,
	); err != nil {
		return uuid.Nil, false, err
	}

	priceChanged := !found ||
		prevSell == nil || *prevSell != int64(item.SellingPrice) ||
		!sameMoney(prevMRP, mrp) || prevCur != item.Currency
	if priceChanged {
		if _, err := s.insertSnapshot(ctx, tx, id, mrp, int64(item.SellingPrice), item.Currency, source); err != nil {
			return uuid.Nil, false, err
		}
	}
	return id, priceChanged, nil
}

// AppendSnapshot records a price observation taken outside the scheduled sync.
func (s *Store) AppendSnapshot(ctx context.Context, tx pgx.Tx, courseID uuid.UUID, mrp *money.Minor, selling money.Minor, currency, source string) (uuid.UUID, error) {
	var m *int64
	if mrp != nil {
		v := int64(*mrp)
		m = &v
	}
	return s.insertSnapshot(ctx, tx, courseID, m, int64(selling), currency, source)
}

func (s *Store) insertSnapshot(ctx context.Context, tx pgx.Tx, courseID uuid.UUID, mrp *int64, selling int64, currency, source string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx, `INSERT INTO course_price_snapshots
		(id, course_id, mrp_amount, selling_price_amount, currency, source)
		VALUES ($1,$2,$3,$4,$5,$6)`, id, courseID, mrp, selling, currency, source)
	return id, err
}

// LatestSnapshot returns the most recent snapshot id for a course.
func (s *Store) LatestSnapshot(ctx context.Context, tx pgx.Tx, courseID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM course_price_snapshots
		WHERE course_id = $1 ORDER BY captured_at DESC LIMIT 1`, courseID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	return id, err
}

// ApplyLivePrice updates the projection from a live provider read and records a
// snapshot when the commercial values moved.
func (s *Store) ApplyLivePrice(ctx context.Context, tx pgx.Tx, c Course, state provider.CommercialState, source string) (snapshotID uuid.UUID, changed bool, err error) {
	var mrp *int64
	if state.MRP != nil {
		v := int64(*state.MRP)
		mrp = &v
	}
	var prevMRP *int64
	if c.MRP != nil {
		v := int64(*c.MRP)
		prevMRP = &v
	}

	changed = c.SellingPrice != state.SellingPrice || !sameMoney(prevMRP, mrp) || c.Currency != state.Currency

	if _, err = tx.Exec(ctx, `UPDATE courses SET selling_price_amount = $2, mrp_amount = $3,
		currency = $4, provider_active = $5, last_synced_at = now(), updated_at = now()
		WHERE id = $1`, c.ID, int64(state.SellingPrice), mrp, state.Currency, state.Active); err != nil {
		return uuid.Nil, false, err
	}

	if changed {
		snapshotID, err = s.insertSnapshot(ctx, tx, c.ID, mrp, int64(state.SellingPrice), state.Currency, source)
		return snapshotID, true, err
	}
	snapshotID, err = s.LatestSnapshot(ctx, tx, c.ID)
	return snapshotID, false, err
}

// DeactivateMissing marks courses the provider no longer lists. It must only run
// after a complete successful scan, otherwise a partial failure would hide the catalog.
func (s *Store) DeactivateMissing(ctx context.Context, tx pgx.Tx, providerCode string, seen []string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE courses SET provider_active = FALSE, updated_at = now()
		WHERE provider = $1 AND provider_active AND NOT (provider_plan_id = ANY($2))`,
		providerCode, seen)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StalestSync reports how long ago the provider catalog was last fully refreshed.
func (s *Store) LastSuccessfulSync(ctx context.Context, providerCode string) (time.Time, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT max(finished_at) FROM provider_sync_runs
		WHERE provider = $1 AND status = 'SUCCEEDED'`, providerCode).Scan(&at)
	if err != nil || at == nil {
		return time.Time{}, err
	}
	return *at, nil
}

func sameMoney(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
