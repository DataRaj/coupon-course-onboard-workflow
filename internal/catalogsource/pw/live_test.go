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

// Opt-in browser check for listing discovery followed by one detail extraction.
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
	targets, e := b.Discover(ctx, uuid.New())
	if e != nil {
		t.Fatal(e)
	}
	if len(targets) == 0 {
		t.Fatal("no public batch targets discovered")
	}
	d, e := b.Acquire(ctx, uuid.New(), targets[0])
	if e != nil {
		t.Fatal(e)
	}
	o, w, e := Normalize(d)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("validated title=%q external_id=%s price_minor=%v provenance=%v warnings=%v", o.Title, o.ExternalID, o.SellingPriceMinor, d.Provenance, w)
}
