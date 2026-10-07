package pw

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"course-coupon/internal/config"
	"github.com/google/uuid"
)

// Opt-in browser check. The worker command additionally persists and logs course_id.
func TestLiveBrowser(t *testing.T) {
	if os.Getenv("PW_LIVE_TEST") != "1" {
		t.Skip("set PW_LIVE_TEST=1 for public PW Chromium acquisition")
	}
	cfg, e := config.LoadPW()
	if e != nil {
		t.Fatal(e)
	}
	b := NewBrowser(cfg, slog.Default())
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, e := b.Acquire(ctx, uuid.New())
	if e != nil {
		t.Fatal(e)
	}
	o, w, e := Normalize(d, cfg.ExpectedTitle)
	if e != nil {
		t.Fatal(e)
	}
	if o.SellingPrice == nil {
		t.Fatalf("incomplete commercial observation: %v", w)
	}
	t.Logf("validated title=%q external_id=%s price_minor=%d provenance=%v warnings=%v", o.Title, o.ExternalID, *o.SellingPrice, d.Provenance, w)
}
