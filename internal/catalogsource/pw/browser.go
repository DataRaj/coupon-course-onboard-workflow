package pw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"course-coupon/internal/config"
	"github.com/google/uuid"
	playwright "github.com/mxschmitt/playwright-go"
)

// Browser owns a reusable runtime/process. Acquire uses a fresh anonymous context.
// Close must be called after all acquisitions; cancellation stops active work.
type Browser struct {
	cfg     config.PWConfig
	log     *slog.Logger
	runtime *playwright.Playwright
	browser playwright.Browser
	gate    chan struct{}
}

func NewBrowser(cfg config.PWConfig, log *slog.Logger) *Browser {
	return &Browser{cfg: cfg, log: log, gate: make(chan struct{}, 1)}
}
func (b *Browser) Close() error {
	var err error
	if b.browser != nil {
		err = b.browser.Close()
		b.browser = nil
	}
	if b.runtime != nil {
		err = errors.Join(err, b.runtime.Stop())
		b.runtime = nil
	}
	return err
}
func (b *Browser) start(ctx context.Context) error {
	if b.browser != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return failure("cancelled", err)
	}
	// Run only starts an installed driver; installation is a separate manual step.
	type started struct {
		p *playwright.Playwright
		e error
	}
	ready := make(chan started)
	go func() {
		p, e := playwright.Run()
		select {
		case ready <- started{p, e}:
		case <-ctx.Done():
			if p != nil {
				_ = p.Stop()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return failure("cancelled", ctx.Err())
	case r := <-ready:
		if r.e != nil {
			return failure("driver_unavailable", fmt.Errorf("Playwright driver could not start; run the documented install command"))
		}
		b.runtime = r.p
	}
	// A cancelled launch shuts down the driver and unblocks the pending RPC.
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = b.runtime.Stop(); close(stopped) })
	browser, err := b.runtime.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(b.cfg.Headless), Timeout: playwright.Float(float64(b.cfg.NavigationTimeout.Milliseconds()))})
	if !stop() {
		<-stopped
	}
	if err != nil {
		_ = b.Close()
		if ctx.Err() != nil {
			return failure("cancelled", ctx.Err())
		}
		return failure("browser_startup", fmt.Errorf("Chromium could not launch; verify installation and OS dependencies"))
	}
	b.browser = browser
	return nil
}

func candidateURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" {
		return false
	}
	if u.Hostname() != "api.pw.live" && u.Hostname() != "www.pw.live" {
		return false
	}
	return strings.Contains(u.Path, "/batches/") && !strings.Contains(u.Path, "/user") && !strings.Contains(u.Path, "/purchase")
}

type networkObserver struct {
	mu         sync.Mutex
	responses  []playwright.Response
	requests   int
	discovered int
}

func (n *networkObserver) attach(page playwright.Page) {
	page.OnRequest(func(r playwright.Request) {
		if candidateURL(r.URL()) {
			n.mu.Lock()
			n.requests++
			n.mu.Unlock()
		}
	})
	page.OnResponse(func(r playwright.Response) {
		if candidateURL(r.URL()) && strings.Contains(r.Headers()["content-type"], "application/json") {
			n.mu.Lock()
			n.discovered++
			n.mu.Unlock()
		}
	})
	// Completion means Body is available; no unbounded read inside an event callback.
	page.OnRequestFinished(func(r playwright.Request) {
		if !candidateURL(r.URL()) {
			return
		}
		// Do not consume authenticated catalog responses, even if page code sends one.
		if r.Headers()["authorization"] != "" {
			return
		}
		response, e := r.Response()
		if e != nil || response == nil || response.Status() != 200 {
			return
		}
		headers := response.Headers()
		if !strings.Contains(headers["content-type"], "application/json") {
			return
		}
		if size, _ := strconv.ParseInt(headers["content-length"], 10, 64); size > maxPayloadBytes {
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if len(n.responses) < 8 {
			n.responses = append(n.responses, response)
		}
	})
}
func (n *networkObserver) payloads() ([]Payload, []string, int, int) {
	n.mu.Lock()
	rs := append([]playwright.Response{}, n.responses...)
	requests := n.requests
	discovered := n.discovered
	n.mu.Unlock()
	var result []Payload
	var warnings []string
	for _, r := range rs {
		raw, e := r.Body()
		if e != nil {
			warnings = append(warnings, "network_body_unavailable")
			continue
		}
		if len(raw) > maxPayloadBytes {
			warnings = append(warnings, "network_body_too_large")
			continue
		}
		result = append(result, Payload{Body: raw, Source: "network_json"})
	}
	return result, warnings, requests, discovered
}

// Acquire performs no database or commerce work.
func (b *Browser) Acquire(parent context.Context, runID uuid.UUID) (dto PWBatchDTO, err error) {
	ctx, cancel := context.WithTimeout(parent, b.cfg.Timeout)
	defer cancel()
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-ctx.Done():
		return dto, failure("cancelled", ctx.Err())
	}
	if err = b.start(ctx); err != nil {
		return dto, err
	}
	// Stop the driver at the overall deadline, including protocol operations that
	// do not expose a Playwright timeout (NewContext, Body, Evaluate and tracing).
	runtime := b.runtime
	shutdownDone := make(chan struct{})
	stopRuntime := context.AfterFunc(ctx, func() { _ = runtime.Stop(); close(shutdownDone) })
	defer func() {
		if !stopRuntime() {
			<-shutdownDone
			b.runtime = nil
			b.browser = nil
		}
	}()
	bc, e := b.browser.NewContext(playwright.BrowserNewContextOptions{Locale: playwright.String("en-IN"), ServiceWorkers: playwright.ServiceWorkerPolicyBlock})
	if e != nil {
		if ctx.Err() != nil {
			return dto, failure("cancelled", ctx.Err())
		}
		return dto, failure("context_creation", fmt.Errorf("could not create isolated browser context"))
	}
	var closeOnce sync.Once
	closeContext := func() { closeOnce.Do(func() { _ = bc.Close() }) }
	stop := context.AfterFunc(ctx, closeContext)
	defer func() {
		stop()
		closeContext()
		if ctx.Err() != nil {
			err = failure("cancelled", ctx.Err())
		}
	}()
	bc.SetDefaultTimeout(float64(b.cfg.NavigationTimeout.Milliseconds()))
	bc.SetDefaultNavigationTimeout(float64(b.cfg.NavigationTimeout.Milliseconds()))
	if b.cfg.Trace {
		if e = prepareTraces(b.cfg.TraceDir); e != nil {
			return dto, failure("trace_setup", e)
		}
		if e = bc.Tracing().Start(playwright.TracingStartOptions{Screenshots: playwright.Bool(true), Snapshots: playwright.Bool(true), Sources: playwright.Bool(false)}); e != nil {
			return dto, failure("trace_setup", fmt.Errorf("could not start tracing"))
		}
		defer func() {
			trace := filepath.Join(b.cfg.TraceDir, "pw-"+runID.String()+".zip")
			if e := bc.Tracing().Stop(trace); e != nil {
				b.log.Warn("PW trace unavailable", "ingestion_run_id", runID)
			} else {
				b.log.Info("PW trace saved", "trace_path", trace)
			}
		}()
	}
	page, e := bc.NewPage()
	if e != nil {
		return dto, failure("page_creation", fmt.Errorf("could not create page"))
	}
	var crashed atomic.Bool
	var consoleErrors atomic.Int32
	page.OnCrash(func(playwright.Page) { crashed.Store(true) })
	page.OnConsole(func(m playwright.ConsoleMessage) {
		if m.Type() == "error" {
			consoleErrors.Add(1)
		}
	})
	var observer networkObserver
	observer.attach(page)
	response, e := page.Goto(b.cfg.BatchURL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded})
	if e != nil {
		return dto, failure("navigation_failed", fmt.Errorf("public batch navigation failed or timed out"))
	}
	if response == nil || response.Status() < 200 || response.Status() >= 300 {
		return dto, failure("http_status", fmt.Errorf("public batch returned a non-success response"))
	}
	_, canonical, e := targetIdentity(page.URL())
	if e != nil {
		return dto, failure("unexpected_redirect", fmt.Errorf("navigation left configured public batch"))
	}
	_, expected, _ := targetIdentity(b.cfg.BatchURL)
	if canonical != expected {
		return dto, failure("unexpected_redirect", fmt.Errorf("navigation changed batch identity"))
	}
	b.log.InfoContext(ctx, "PW navigation complete", "source_url", canonical, "ingestion_run_id", runID, "browser", "chromium")
	title := page.GetByRole(*playwright.AriaRoleHeading, playwright.PageGetByRoleOptions{Level: playwright.Int(1)}).First()
	if e = title.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); e != nil {
		return dto, failure("page_incomplete", fmt.Errorf("batch heading did not become visible"))
	}
	// Wait on the base-plan section, not network-idle or a fixed sleep.
	priceLocator := page.Locator("#features").GetByText(regexp.MustCompile(`^₹[0-9,]+(?:\.[0-9]{1,2})?$`)).First()
	var readinessWarnings []string
	if e = priceLocator.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); e != nil {
		readinessWarnings = append(readinessWarnings, "base_plan_price_not_rendered")
	}
	if ctx.Err() != nil {
		return dto, failure("cancelled", ctx.Err())
	}
	if crashed.Load() {
		return dto, failure("page_crash", fmt.Errorf("Chromium page crashed"))
	}
	dom, e := readDOM(page)
	if e != nil {
		return dto, failure("dom_extraction", fmt.Errorf("could not read rendered batch identity"))
	}
	payloads, warnings, requests, discovered := observer.payloads()
	embedded, e := page.Locator(`script[type="application/json"], script[type="application/ld+json"]`).AllTextContents()
	if e != nil {
		warnings = append(warnings, "embedded_json_unavailable")
	}
	// This reads already-hydrated public state; it never evals script source.
	flightValue, e := page.Evaluate(`() => (self.__next_f || []).filter(x => Array.isArray(x) && x[0] === 1 && typeof x[1] === 'string').map(x => x[1]).join('').slice(0, 4194304)`)
	flight, _ := flightValue.(string)
	if e != nil {
		warnings = append(warnings, "embedded_flight_unavailable")
	}
	dto, e = Extract(canonical, time.Now().UTC(), payloads, embedded, flight, dom)
	dto.Warnings = append(dto.Warnings, readinessWarnings...)
	dto.Warnings = append(dto.Warnings, warnings...)
	b.log.InfoContext(ctx, "PW extraction complete", "ingestion_run_id", runID, "relevant_requests", requests, "json_responses_discovered", discovered, "candidate_bodies_read", len(payloads), "network_payload_used", hasSource(dto, "network_json"), "dom_used", hasSource(dto, "rendered_dom"), "fields_extracted", len(dto.Provenance), "console_errors", consoleErrors.Load(), "warnings", dto.Warnings)
	return dto, e
}
func hasSource(d PWBatchDTO, source string) bool {
	for _, s := range d.Provenance {
		if s == source {
			return true
		}
	}
	return false
}

// All PW selectors and DOM relationships live here. IDs are semantic sections;
// the purchase card is located relative to its exact CTA rather than CSS hashes.
func readDOM(page playwright.Page) (DOMState, error) {
	var d DOMState
	var err error
	d.Title, err = page.GetByRole(*playwright.AriaRoleHeading, playwright.PageGetByRoleOptions{Level: playwright.Int(1)}).First().InnerText()
	if err != nil {
		return d, err
	}
	optionalText := func(l playwright.Locator) string {
		if n, e := l.Count(); e == nil && n == 1 {
			s, _ := l.InnerText()
			return s
		}
		return ""
	}
	d.BasePlan = optionalText(page.Locator("#features"))
	d.About = optionalText(page.Locator("#about"))
	card := page.GetByText("Continue with Batch", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).Locator(`xpath=ancestor::div[.//*[starts-with(normalize-space(text()),'₹')]][1]`)
	d.PurchaseCard = optionalText(card)
	if n, _ := card.Count(); n == 1 {
		v, e := card.Evaluate(`el => { const values = [...el.querySelectorAll('*')].filter(x => x.children.length === 0 && /^₹[\d,.]+$/.test(x.textContent.trim()) && (() => { for (let a=x; a && a!==el; a=a.parentElement) { if (getComputedStyle(a).textDecorationLine.includes('line-through')) return true; } return false; })()).map(x => x.textContent.trim()); return [...new Set(values)].length === 1 ? values[0] : ''; }`, nil)
		if e == nil {
			d.OriginalPrice, _ = v.(string)
		}
	}
	d.Teachers, _ = page.Locator(`#teachers [aria-roledescription="slide"]`).AllInnerTexts()
	return d, nil
}

// Manual traces are opt-in and retain at most five archives in this directory.
func prepareTraces(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(dir, "pw-*.zip"))
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		a, e := os.Stat(entries[i])
		if e != nil {
			return false
		}
		b, e := os.Stat(entries[j])
		return e == nil && a.ModTime().Before(b.ModTime())
	})
	for len(entries) >= 5 {
		if err := os.Remove(entries[0]); err != nil {
			return err
		}
		entries = entries[1:]
	}
	return nil
}
