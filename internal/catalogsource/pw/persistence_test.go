package pw

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"course-coupon/internal/catalogsource"
	"course-coupon/internal/config"
	"course-coupon/internal/course"
	"course-coupon/internal/database"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeAcquirer struct {
	dto    PWBatchDTO
	err    error
	cancel context.CancelFunc
}

func (f *fakeAcquirer) Acquire(context.Context, uuid.UUID) (PWBatchDTO, error) {
	if f.cancel != nil {
		f.cancel()
	}
	return f.dto, f.err
}
func testDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	uri := os.Getenv("PW_TEST_DATABASE_URL")
	if uri == "" {
		t.Skip("set PW_TEST_DATABASE_URL for isolated-schema PostgreSQL tests")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, uri)
	if e != nil {
		t.Fatal(e)
	}
	schema := "pw_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(uri)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, e := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		if e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	if e = database.Migrate(ctx, pool); e != nil {
		t.Fatal(e)
	}
	return pool
}
func TestPersistenceVerticalSlice(t *testing.T) {
	pool := testDatabase(t)
	ctx := context.Background()
	dto, e := Extract(fixtureURL, time.Now().UTC(), nil, []string{string(fixture(t, "batch.json"))}, "", fixtureDOM(t))
	if e != nil {
		t.Fatal(e)
	}
	source := &fakeAcquirer{dto: dto}
	log := slog.New(slog.DiscardHandler)
	service := Service{Store: catalogsource.NewStore(pool), Source: source, Config: config.PWConfig{BatchURL: fixtureURL, ExtractorVersion: Version}, Log: log}
	count := func(table string) int {
		t.Helper()
		var n int
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	read := func(id uuid.UUID) course.Course {
		t.Helper()
		c, e := course.NewStore(pool).Get(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	// An incomplete first observation must never create a zero-priced course.
	source.dto.SellingPrice = ""
	id, e := service.IngestTargetBatch(ctx)
	if !errors.Is(e, ErrIncomplete) || id != uuid.Nil || count("courses") != 0 {
		t.Fatalf("initial incomplete: id=%s error=%v", id, e)
	}
	source.dto = dto
	id, e = service.IngestTargetBatch(ctx)
	if e != nil {
		t.Fatal(e)
	}
	first := read(id)
	if first.SellingPrice != 389920 || first.MRP == nil || *first.MRP != 630000 {
		t.Fatalf("first price: %+v", first)
	}
	source.dto.AcquiredAt = dto.AcquiredAt.Add(time.Minute)
	repeated, e := service.IngestTargetBatch(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if repeated != id || count("courses") != 1 || count("catalog_sources") != 1 || count("course_price_snapshots") != 1 {
		t.Fatal("duplicate ingestion produced duplicate state/history")
	}
	if !read(id).UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatal("same data rewrote projection")
	}
	var successful time.Time
	if e = pool.QueryRow(ctx, `SELECT last_successful_at FROM catalog_sources`).Scan(&successful); e != nil {
		t.Fatal(e)
	}
	current := read(id)
	source.dto.SellingPrice = "broken"
	source.dto.Title = "Unexpected partial title"
	_, e = service.IngestTargetBatch(ctx)
	if !errors.Is(e, ErrIncomplete) {
		t.Fatalf("missing incomplete: %v", e)
	}
	stale := read(id)
	if stale.Title != first.Title || stale.SellingPrice != first.SellingPrice || !stale.ProviderActive || stale.CommercialStatus != "STALE" || !stale.CommercialObservedAt.Equal(*current.CommercialObservedAt) {
		t.Fatalf("lost last-known-good state: %+v", stale)
	}
	var after time.Time
	if e = pool.QueryRow(ctx, `SELECT last_successful_at FROM catalog_sources`).Scan(&after); e != nil || !after.Equal(successful) {
		t.Fatal("incomplete changed successful timestamp", e)
	}
	source.err = failure("page_incomplete", errors.New("simulated layout failure"))
	_, e = service.IngestTargetBatch(ctx)
	if e == nil {
		t.Fatal("expected scrape failure")
	}
	if c := read(id); !c.ProviderActive || c.SellingPrice != 389920 {
		t.Fatal("failed scrape corrupted projection")
	}
	// A valid changed price appends history and preserves the original snapshot.
	source.err = nil
	source.dto = dto
	source.dto.SellingPrice = "4099.20"
	source.dto.AcquiredAt = time.Now().UTC()
	changedID, e := service.IngestTargetBatch(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if changedID != id || read(id).SellingPrice != 409920 || count("course_price_snapshots") != 2 {
		t.Fatal("price change not recorded")
	}
	var oldPrice int64
	var snapshotSource string
	if e = pool.QueryRow(ctx, `SELECT selling_price_amount,source FROM course_price_snapshots ORDER BY captured_at LIMIT 1`).Scan(&oldPrice, &snapshotSource); e != nil {
		t.Fatal(e)
	}
	if oldPrice != 389920 || snapshotSource != course.SourceCatalogIngestion {
		t.Fatal("historical snapshot changed")
	}
	_, e = service.IngestTargetBatch(ctx)
	if e != nil || count("course_price_snapshots") != 2 {
		t.Fatal("same changed price duplicated history", e)
	}
	// Actual existing read handler exposes normalized state only.
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { course.NewHandler(course.NewStore(pool), log).Routes(r) })
	for _, path := range []string{"/api/v1/courses/" + id.String(), "/api/v1/courses?class=12&target_exam=IIT-JEE"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if recorder.Code != 200 || !strings.Contains(body, `"selling_price":4099.2`) || !strings.Contains(body, `"provider":"PW"`) || !strings.Contains(body, `"status":"CURRENT"`) {
			t.Fatalf("read API: %d %s", recorder.Code, body)
		}
		for _, raw := range []string{"embedded_state", "provenance", "selector", "fee", "playwright", "teacherIds", "priceLabel"} {
			if strings.Contains(body, raw) {
				t.Fatalf("raw extraction leaked: %s", raw)
			}
		}
	}
	// Cancellation is still recorded using the bounded cleanup context.
	cancelled, cancel := context.WithCancel(ctx)
	source.cancel = cancel
	source.err = context.Canceled
	_, e = service.IngestTargetBatch(cancelled)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation lost: %v", e)
	}
	var running int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM catalog_ingestion_runs WHERE result='RUNNING'`).Scan(&running); e != nil || running != 0 {
		t.Fatal("run left unfinished", e)
	}
	if count("offers") != 0 || count("wallet_ledger") != 0 || count("redemptions") != 0 {
		t.Fatal("catalog ingestion crossed commerce boundary")
	}
	// Same target cannot execute concurrently across workers.
	slug, _, _ := targetIdentity(fixtureURL)
	h := fnv.New64a()
	_, _ = h.Write([]byte(Provider + ":" + slug))
	locked, release, e := database.TryAdvisoryLock(ctx, pool, int64(h.Sum64()))
	if e != nil || !locked {
		t.Fatal("lock setup", e)
	}
	defer release()
	_, e = service.IngestTargetBatch(ctx)
	if errorCode(e) != "already_running" {
		t.Fatalf("concurrent ingestion not rejected: %v", e)
	}
}
