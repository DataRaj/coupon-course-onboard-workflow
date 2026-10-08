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

// Browser owns one reusable Playwright runtime and Chromium process. Each
// operation uses a fresh anonymous BrowserContext and Page.
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
	type started struct {
		playwright *playwright.Playwright
		err        error
	}
	ready := make(chan started)
	go func() {
		pw, err := playwright.Run()
		select {
		case ready <- started{pw, err}:
		case <-ctx.Done():
			if pw != nil {
				_ = pw.Stop()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return failure("cancelled", ctx.Err())
	case result := <-ready:
		if result.err != nil {
			return failure("driver_unavailable", fmt.Errorf("Playwright driver could not start; install its Chromium runtime first: %w", result.err))
		}
		b.runtime = result.playwright
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = b.runtime.Stop()
		close(stopped)
	})
	browser, err := b.runtime.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(b.cfg.Headless),
		Timeout:  playwright.Float(float64(b.cfg.NavigationTimeout.Milliseconds())),
	})
	if !stop() {
		<-stopped
	}
	if err != nil {
		_ = b.Close()
		if ctx.Err() != nil {
			return failure("cancelled", ctx.Err())
		}
		return failure("browser_startup", fmt.Errorf("Chromium could not launch; verify installation and OS dependencies: %w", err))
	}
	b.browser = browser
	return nil
}

type pageLease struct {
	b             *Browser
	ctx           context.Context
	cancel        context.CancelFunc
	page          playwright.Page
	browserCtx    playwright.BrowserContext
	closeContext  func()
	stopContext   func() bool
	stopRuntime   func() bool
	shutdownDone  chan struct{}
	tracePath     string
	traceStarted  bool
	crashed       atomic.Bool
	consoleErrors atomic.Int32
	closed        atomic.Bool
}

func (l *pageLease) Close() {
	if !l.closed.CompareAndSwap(false, true) {
		return
	}
	l.stopContext()
	if l.traceStarted {
		if err := l.browserCtx.Tracing().Stop(l.tracePath); err != nil {
			l.b.log.Warn("PW trace unavailable", "trace_path", l.tracePath)
		} else {
			l.b.log.Info("PW trace saved", "trace_path", l.tracePath)
		}
	}
	l.closeContext()
	if !l.stopRuntime() {
		<-l.shutdownDone
		l.b.runtime = nil
		l.b.browser = nil
	}
	l.cancel()
	<-l.b.gate
}

func (b *Browser) openPage(parent context.Context, runID uuid.UUID, label string) (*pageLease, error) {
	ctx, cancel := context.WithTimeout(parent, b.cfg.Timeout)
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, failure("cancelled", ctx.Err())
	}
	fail := func(err error) (*pageLease, error) {
		cancel()
		<-b.gate
		return nil, err
	}
	if err := b.start(ctx); err != nil {
		return fail(err)
	}
	runtime := b.runtime
	shutdownDone := make(chan struct{})
	stopRuntime := context.AfterFunc(ctx, func() {
		_ = runtime.Stop()
		close(shutdownDone)
	})
	bc, err := b.browser.NewContext(playwright.BrowserNewContextOptions{
		Locale:         playwright.String("en-IN"),
		ServiceWorkers: playwright.ServiceWorkerPolicyBlock,
	})
	if err != nil {
		if !stopRuntime() {
			<-shutdownDone
			b.runtime, b.browser = nil, nil
		}
		if ctx.Err() != nil {
			return fail(failure("cancelled", ctx.Err()))
		}
		return fail(failure("context_creation", fmt.Errorf("could not create isolated browser context")))
	}
	var closeOnce sync.Once
	closeContext := func() { closeOnce.Do(func() { _ = bc.Close() }) }
	stopContext := context.AfterFunc(ctx, closeContext)
	lease := &pageLease{
		b: b, ctx: ctx, cancel: cancel, browserCtx: bc,
		closeContext: closeContext, stopContext: stopContext,
		stopRuntime: stopRuntime, shutdownDone: shutdownDone,
	}
	bc.SetDefaultTimeout(float64(b.cfg.NavigationTimeout.Milliseconds()))
	bc.SetDefaultNavigationTimeout(float64(b.cfg.NavigationTimeout.Milliseconds()))
	if b.cfg.Trace {
		if err := prepareTraces(b.cfg.TraceDir); err != nil {
			lease.Close()
			return nil, failure("trace_setup", err)
		}
		if err := bc.Tracing().Start(playwright.TracingStartOptions{
			Screenshots: playwright.Bool(true), Snapshots: playwright.Bool(true), Sources: playwright.Bool(false),
		}); err != nil {
			lease.Close()
			return nil, failure("trace_setup", fmt.Errorf("could not start tracing"))
		}
		lease.traceStarted = true
		lease.tracePath = filepath.Join(b.cfg.TraceDir, "pw-"+label+"-"+runID.String()+".zip")
	}
	page, err := bc.NewPage()
	if err != nil {
		lease.Close()
		return nil, failure("page_creation", fmt.Errorf("could not create page"))
	}
	lease.page = page
	page.OnCrash(func(playwright.Page) { lease.crashed.Store(true) })
	page.OnConsole(func(message playwright.ConsoleMessage) {
		if message.Type() == "error" {
			lease.consoleErrors.Add(1)
		}
	})
	return lease, nil
}

func relevantURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	if u.Hostname() != "api.pw.live" && u.Hostname() != "www.pw.live" {
		return false
	}
	path := strings.ToLower(u.Path)
	catalogPath := strings.Contains(path, "/batch") || strings.Contains(path, "/course") || strings.Contains(path, "/cohort")
	return catalogPath && !strings.Contains(path, "/user") && !strings.Contains(path, "/purchase")
}

type networkObserver struct {
	mu         sync.Mutex
	responses  []playwright.Response
	requests   int
	discovered int
	rateLimits int
	serverErrs int
}

func (n *networkObserver) attach(page playwright.Page) {
	page.OnRequest(func(request playwright.Request) {
		if relevantURL(request.URL()) {
			n.mu.Lock()
			n.requests++
			n.mu.Unlock()
		}
	})
	page.OnResponse(func(response playwright.Response) {
		if isPWURL(response.URL()) && response.Status() == 429 {
			n.mu.Lock()
			n.rateLimits++
			n.mu.Unlock()
		}
		if isPWURL(response.URL()) && response.Status() >= 500 {
			n.mu.Lock()
			n.serverErrs++
			n.mu.Unlock()
		}
		if relevantURL(response.URL()) && strings.Contains(response.Headers()["content-type"], "application/json") {
			n.mu.Lock()
			n.discovered++
			n.mu.Unlock()
		}
	})
	page.OnRequestFinished(func(request playwright.Request) {
		if !relevantURL(request.URL()) || request.Headers()["authorization"] != "" {
			return
		}
		response, err := request.Response()
		if err != nil || response == nil || response.Status() != 200 {
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
		if len(n.responses) < 32 {
			n.responses = append(n.responses, response)
		}
	})
}

func isPWURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && (u.Hostname() == "www.pw.live" || u.Hostname() == "api.pw.live")
}

func (n *networkObserver) health() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rateLimits, n.serverErrs
}

// resetBodies keeps each listing's candidate payloads independent while
// retaining run-wide request and response counters.
func (n *networkObserver) resetBodies() {
	n.mu.Lock()
	n.responses = nil
	n.mu.Unlock()
}

func (n *networkObserver) payloads() ([]Payload, []string, int, int) {
	n.mu.Lock()
	responses := append([]playwright.Response{}, n.responses...)
	requests, discovered := n.requests, n.discovered
	n.mu.Unlock()
	var payloads []Payload
	var warnings []string
	for _, response := range responses {
		body, err := response.Body()
		if err != nil {
			warnings = append(warnings, "network_body_unavailable")
			continue
		}
		if len(body) > maxPayloadBytes {
			warnings = append(warnings, "network_body_too_large")
			continue
		}
		payloads = append(payloads, Payload{Body: body, Source: "network_json"})
	}
	return payloads, warnings, requests, discovered
}

// Discover finds detail targets from the configured public listing and its
// directly linked category listings.
func (b *Browser) Discover(parent context.Context, runID uuid.UUID) ([]Target, error) {
	lease, err := b.openPage(parent, runID, "listing")
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	var observer networkObserver
	observer.attach(lease.page)
	hubErr := b.navigateListing(lease, runID, b.cfg.ListingURL, true)
	listingURLs := []string{b.cfg.ListingURL}
	if hubErr == nil {
		allLinks := anchorHrefs(lease.page, `a[href]`, 2_000)
		listingURLs = append(listingURLs, childListingURLs(b.cfg.ListingURL, allLinks)...)
		listingURLs = appendMissingURLs(listingURLs, publicIITJEEListings(b.cfg.ListingURL))
	} else if errorCode(hubErr) == "rate_limited" || b.cfg.ListingURL != "https://www.pw.live/iit-jee/batches" {
		return nil, hubErr
	} else {
		b.log.WarnContext(lease.ctx, "PW hub unavailable; trying public category listings",
			"run_id", runID, "source_url", b.cfg.ListingURL,
			"error_code", errorCode(hubErr), "error", hubErr)
		listingURLs = append(listingURLs, publicIITJEEListings(b.cfg.ListingURL)...)
	}
	targets := make([]Target, 0, b.cfg.MaxBatches)
	seenTargets := map[string]struct{}{}
	var totalRendered int
	var listingWarnings []string
	if hubErr != nil {
		listingWarnings = append(listingWarnings, "hub: "+hubErr.Error())
	}
	processedListings := 0
	for index, listingURL := range listingURLs {
		if index == 0 && hubErr != nil {
			continue
		}
		if index > 0 {
			observer.resetBodies()
			if err := b.navigateListing(lease, runID, listingURL, false); err != nil {
				listingWarnings = append(listingWarnings, listingURL+": "+errorCode(err))
				if errorCode(err) == "rate_limited" {
					return nil, err
				}
				continue
			}
		}
		processedListings++
		hrefs, renderedLinks := b.expandListing(lease, runID, listingURL, b.cfg.MaxBatches*4)
		totalRendered += renderedLinks
		embedded, _ := lease.page.Locator(`script[type="application/json"], script[type="application/ld+json"]`).AllTextContents()
		flightValue, _ := lease.page.Evaluate(`() => (self.__next_f || []).filter(x => Array.isArray(x) && x[0] === 1 && typeof x[1] === 'string').map(x => x[1]).join('').slice(0, 4194304)`)
		flight, _ := flightValue.(string)
		payloads, warnings, _, _ := observer.payloads()
		listingWarnings = append(listingWarnings, warnings...)
		for _, target := range DiscoverTargets(listingURL, payloads, embedded, flight, hrefs, b.cfg.MaxBatches, b.cfg.BatchSlugs) {
			if len(targets) >= b.cfg.MaxBatches {
				break
			}
			if _, exists := seenTargets[target.CanonicalURL]; exists {
				continue
			}
			seenTargets[target.CanonicalURL] = struct{}{}
			targets = append(targets, target)
		}
		b.log.InfoContext(lease.ctx, "PW listing processed",
			"run_id", runID, "listing_url", listingURL, "listing_index", index+1,
			"listing_total", len(listingURLs), "rendered_links", renderedLinks,
			"catalog_targets", len(targets))
	}
	_, _, requests, discovered := observer.payloads()
	rateLimits, serverErrors := observer.health()
	if rateLimits > 0 || serverErrors > 0 {
		b.log.WarnContext(lease.ctx, "PW listing network degradation observed", "run_id", runID,
			"rate_limited_responses", rateLimits, "server_error_responses", serverErrors)
	}
	if lease.ctx.Err() != nil {
		return nil, failure("cancelled", lease.ctx.Err())
	}
	if lease.crashed.Load() {
		return nil, failure("page_crash", fmt.Errorf("Chromium listing page crashed"))
	}
	b.log.InfoContext(lease.ctx, "PW listing discovery complete",
		"source_url", b.cfg.ListingURL, "run_id", runID,
		"listings_discovered", len(listingURLs), "listings_processed", processedListings,
		"target_limit", b.cfg.MaxBatches, "target_limit_reached", len(targets) >= b.cfg.MaxBatches,
		"rate_limited_responses", rateLimits, "server_error_responses", serverErrors,
		"relevant_requests", requests, "json_responses_discovered", discovered,
		"rendered_links", totalRendered, "targets", len(targets),
		"console_errors", lease.consoleErrors.Load(), "warnings", listingWarnings)
	if len(targets) == 0 {
		return nil, failure("no_targets", fmt.Errorf("%d public listings processed but no valid batch detail links found; warnings: %s", processedListings, strings.Join(listingWarnings, "; ")))
	}
	return targets, nil
}

func (b *Browser) navigateListing(lease *pageLease, runID uuid.UUID, listingURL string, required bool) error {
	started := time.Now()
	b.log.InfoContext(lease.ctx, "PW listing navigation started", "run_id", runID, "source_url", listingURL)
	response, err := lease.page.Goto(listingURL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateCommit})
	if err != nil {
		b.log.WarnContext(lease.ctx, "PW listing navigation failed", "run_id", runID,
			"source_url", listingURL, "page_url", lease.page.URL(),
			"duration_ms", time.Since(started).Milliseconds(), "error", err)
		return failure("listing_navigation_failed", fmt.Errorf("navigate %s: %w", listingURL, err))
	}
	if response == nil {
		return failure("listing_http_status", fmt.Errorf("public listing returned no main response"))
	}
	status := response.Status()
	if status == 429 {
		b.log.ErrorContext(lease.ctx, "PW rate limit observed", "run_id", runID,
			"phase", "listing", "source_url", listingURL, "http_status", status,
			"retry_after", response.Headers()["retry-after"])
		return failure("rate_limited", fmt.Errorf("PW returned HTTP 429"))
	}
	if status < 200 || status >= 300 {
		level := slog.LevelWarn
		if required {
			level = slog.LevelError
		}
		b.log.Log(lease.ctx, level, "PW listing request rejected", "run_id", runID,
			"source_url", listingURL, "http_status", status)
		return failure("listing_http_status", fmt.Errorf("public listing returned HTTP %d", status))
	}
	if strings.TrimRight(lease.page.URL(), "/") != listingURL {
		return failure("listing_unexpected_redirect", fmt.Errorf("navigation left configured public listing"))
	}
	if err := lease.page.GetByRole(*playwright.AriaRoleHeading, playwright.PageGetByRoleOptions{Level: playwright.Int(1)}).First().WaitFor(
		playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible, Timeout: playwright.Float(10_000)},
	); err != nil {
		return failure("listing_incomplete", fmt.Errorf("listing heading did not become visible at %s: %w", listingURL, err))
	}
	b.log.InfoContext(lease.ctx, "PW listing navigation complete", "run_id", runID,
		"source_url", listingURL, "http_status", status, "duration_ms", time.Since(started).Milliseconds())
	return nil
}

func anchorHrefs(page playwright.Page, selector string, limit int) []string {
	links := page.Locator(selector)
	count, _ := links.Count()
	if count > limit {
		count = limit
	}
	result := make([]string, 0, count)
	for index := 0; index < count; index++ {
		if href, err := links.Nth(index).GetAttribute("href"); err == nil && strings.TrimSpace(href) != "" {
			result = append(result, href)
		}
	}
	return result
}

func childListingURLs(root string, hrefs []string) []string {
	base, err := url.Parse(root)
	if err != nil {
		return nil
	}
	prefix := strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/batches") + "/"
	seen := map[string]struct{}{root: {}}
	var result []string
	for _, href := range hrefs {
		reference, parseErr := url.Parse(strings.TrimSpace(href))
		if parseErr != nil {
			continue
		}
		candidate := base.ResolveReference(reference)
		candidate.RawQuery, candidate.Fragment = "", ""
		value := strings.TrimRight(candidate.String(), "/")
		if candidate.Scheme != "https" || candidate.Host != "www.pw.live" ||
			!strings.HasPrefix(candidate.Path, prefix) || !strings.HasSuffix(strings.TrimRight(candidate.Path, "/"), "/batches") {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == 12 {
			break
		}
	}
	return result
}

func publicIITJEEListings(root string) []string {
	if root != "https://www.pw.live/iit-jee/batches" {
		return nil
	}
	return []string{
		"https://www.pw.live/iit-jee/class-11/batches",
		"https://www.pw.live/iit-jee/class-12/batches",
		"https://www.pw.live/iit-jee/dropper/batches",
	}
}

func appendMissingURLs(existing, candidates []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, found := seen[candidate]; found {
			continue
		}
		existing = append(existing, candidate)
		seen[candidate] = struct{}{}
	}
	return existing
}

// expandListing collects unique links across bounded lazy-load/"more" passes.
// Each pass waits for a new href instead of relying on a fixed sleep.
func (b *Browser) expandListing(lease *pageLease, runID uuid.UUID, listingURL string, maxLinks int) ([]string, int) {
	page := lease.page
	links := page.Locator(`a[href*="/batches/"]`)
	_ = links.First().WaitFor(playwright.LocatorWaitForOptions{
		State: playwright.WaitForSelectorStateAttached, Timeout: playwright.Float(5_000),
	})
	seen := map[string]struct{}{}
	hrefs := make([]string, 0, maxLinks)
	collect := func() int {
		count, _ := links.Count()
		for index := 0; index < count && len(hrefs) < maxLinks; index++ {
			href, err := links.Nth(index).GetAttribute("href")
			if err != nil || href == "" {
				continue
			}
			if _, found := seen[href]; found {
				continue
			}
			seen[href] = struct{}{}
			hrefs = append(hrefs, href)
		}
		return count
	}
	collect()
	stablePasses := 0
	for pass := 0; pass < 50 && len(hrefs) < maxLinks && stablePasses < 2; pass++ {
		before := append([]string{}, hrefs...)
		clicked, controlFound, controlText, clickError := false, false, "", ""
		textMatch := page.GetByText(regexp.MustCompile(`(?i)^\s*(?:load\s+more(?:\s+(?:batches|courses))?|show\s+more(?:\s+(?:batches|courses))?|view\s+more\s+(?:batches|courses))\s*$`))
		more := textMatch.Locator(`xpath=ancestor-or-self::*[self::button or self::a or @role='button'][1]`)
		count, _ := more.Count()
		if count == 0 {
			more = textMatch
			count, _ = more.Count()
		}
		if count > 0 {
			for index := 0; index < count; index++ {
				control := more.Nth(index)
				if visible, _ := control.IsVisible(); !visible {
					continue
				}
				controlFound = true
				controlText, _ = control.InnerText()
				_ = control.ScrollIntoViewIfNeeded()
				if err := control.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5_000)}); err == nil {
					clicked = true
				} else {
					clickError = "load_more_click_failed"
				}
				break
			}
		}
		if !clicked {
			_, _ = page.Evaluate(`() => window.scrollTo(0, document.body.scrollHeight)`)
		}
		_, _ = page.WaitForFunction(
			`previous => [...document.querySelectorAll('a[href*="/batches/"]')].some(a => !previous.includes(a.getAttribute('href')))`,
			before,
			playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8_000)},
		)
		previousCount := len(hrefs)
		collect()
		b.log.InfoContext(lease.ctx, "PW listing expansion pass",
			"run_id", runID, "listing_url", listingURL, "pass", pass+1,
			"action", map[bool]string{true: "click_more", false: "scroll"}[clicked],
			"control_found", controlFound, "control_text", strings.TrimSpace(controlText),
			"click_error", clickError, "links_before", previousCount,
			"links_after", len(hrefs), "new_links", len(hrefs)-previousCount)
		if len(hrefs) == previousCount {
			stablePasses++
		} else {
			stablePasses = 0
		}
	}
	return hrefs, len(seen)
}

// Acquire extracts one immutable target and performs no database or commerce work.
func (b *Browser) Acquire(parent context.Context, runID uuid.UUID, target Target) (PWBatchDTO, error) {
	var dto PWBatchDTO
	slug, canonicalTarget, err := targetIdentity(target.CanonicalURL)
	if err != nil || slug != target.Slug {
		return dto, failure("target_invalid", fmt.Errorf("target identity is invalid"))
	}
	lease, err := b.openPage(parent, runID, slug)
	if err != nil {
		return dto, err
	}
	defer lease.Close()
	var observer networkObserver
	observer.attach(lease.page)
	navigationStarted := time.Now()
	response, err := lease.page.Goto(canonicalTarget, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateCommit})
	if err != nil {
		b.log.WarnContext(lease.ctx, "PW batch navigation failed", "run_id", runID,
			"source_url", canonicalTarget, "page_url", lease.page.URL(),
			"duration_ms", time.Since(navigationStarted).Milliseconds(), "error", err)
		return dto, failure("navigation_failed", fmt.Errorf("navigate %s: %w", canonicalTarget, err))
	}
	if response != nil && response.Status() == 429 {
		b.log.ErrorContext(lease.ctx, "PW rate limit observed", "run_id", runID,
			"phase", "batch_detail", "source_url", canonicalTarget, "http_status", 429,
			"retry_after", response.Headers()["retry-after"])
		return dto, failure("rate_limited", fmt.Errorf("PW returned HTTP 429"))
	}
	if response == nil || response.Status() < 200 || response.Status() >= 300 {
		status := 0
		if response != nil {
			status = response.Status()
		}
		b.log.WarnContext(lease.ctx, "PW batch request rejected", "run_id", runID,
			"phase", "batch_detail", "source_url", canonicalTarget, "http_status", status)
		return dto, failure("http_status", fmt.Errorf("public batch returned HTTP %d", status))
	}
	_, finalURL, err := targetIdentity(lease.page.URL())
	if err != nil || finalURL != canonicalTarget {
		return dto, failure("unexpected_redirect", fmt.Errorf("navigation changed batch identity"))
	}
	b.log.InfoContext(lease.ctx, "PW navigation complete", "source_url", finalURL,
		"run_id", runID, "browser", "chromium", "http_status", response.Status(),
		"duration_ms", time.Since(navigationStarted).Milliseconds())
	title := lease.page.GetByRole(*playwright.AriaRoleHeading, playwright.PageGetByRoleOptions{Level: playwright.Int(1)}).First()
	if err = title.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); err != nil {
		return dto, failure("page_incomplete", fmt.Errorf("batch heading did not become visible"))
	}
	priceLocator := lease.page.Locator("#features").GetByText(regexp.MustCompile(`^₹[0-9,]+(?:\.[0-9]{1,2})?$`)).First()
	var readinessWarnings []string
	if err = priceLocator.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(3_000),
	}); err != nil {
		readinessWarnings = append(readinessWarnings, "base_plan_price_not_rendered")
	}
	if lease.ctx.Err() != nil {
		return dto, failure("cancelled", lease.ctx.Err())
	}
	if lease.crashed.Load() {
		return dto, failure("page_crash", fmt.Errorf("Chromium detail page crashed"))
	}
	dom, err := readDOM(lease.page)
	if err != nil {
		return dto, failure("dom_extraction", fmt.Errorf("could not read rendered batch identity"))
	}
	payloads, warnings, requests, discovered := observer.payloads()
	rateLimits, serverErrors := observer.health()
	if rateLimits > 0 {
		warnings = append(warnings, "network_rate_limited")
	}
	if serverErrors > 0 {
		warnings = append(warnings, "network_server_error")
	}
	embedded, embeddedErr := lease.page.Locator(`script[type="application/json"], script[type="application/ld+json"]`).AllTextContents()
	if embeddedErr != nil {
		warnings = append(warnings, "embedded_json_unavailable")
	}
	flightValue, flightErr := lease.page.Evaluate(`() => (self.__next_f || []).filter(x => Array.isArray(x) && x[0] === 1 && typeof x[1] === 'string').map(x => x[1]).join('').slice(0, 4194304)`)
	flight, _ := flightValue.(string)
	if flightErr != nil {
		warnings = append(warnings, "embedded_flight_unavailable")
	}
	dto, err = Extract(finalURL, nowUTC(), payloads, embedded, flight, dom)
	dto.Warnings = append(dto.Warnings, readinessWarnings...)
	dto.Warnings = append(dto.Warnings, warnings...)
	b.log.InfoContext(lease.ctx, "PW extraction complete",
		"run_id", runID, "source_url", finalURL, "relevant_requests", requests,
		"json_responses_discovered", discovered, "candidate_bodies_read", len(payloads),
		"network_payload_used", hasSource(dto, "network_json"), "dom_used", hasSource(dto, "rendered_dom"),
		"fields_extracted", len(dto.Provenance), "thumbnail_extracted", dto.Thumbnail != "",
		"rate_limited_responses", rateLimits, "server_error_responses", serverErrors,
		"console_errors", lease.consoleErrors.Load(), "warnings", dto.Warnings)
	return dto, err
}

var nowUTC = func() time.Time { return time.Now().UTC() }

func hasSource(dto PWBatchDTO, source string) bool {
	for _, current := range dto.Provenance {
		if current == source {
			return true
		}
	}
	return false
}

func readDOM(page playwright.Page) (DOMState, error) {
	var state DOMState
	title, err := page.GetByRole(*playwright.AriaRoleHeading, playwright.PageGetByRoleOptions{Level: playwright.Int(1)}).First().InnerText()
	if err != nil {
		return state, err
	}
	state.Title = title
	if value, attributeErr := page.Locator(`meta[property="og:image"]`).First().GetAttribute("content"); attributeErr == nil {
		state.Thumbnail = value
	}
	optionalText := func(locator playwright.Locator) string {
		if count, countErr := locator.Count(); countErr == nil && count == 1 {
			value, _ := locator.InnerText()
			return value
		}
		return ""
	}
	state.BasePlan = optionalText(page.Locator("#features"))
	state.About = optionalText(page.Locator("#about"))
	card := page.GetByText("Continue with Batch", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)}).Locator(`xpath=ancestor::div[.//*[starts-with(normalize-space(text()),'₹')]][1]`)
	state.PurchaseCard = optionalText(card)
	if count, _ := card.Count(); count == 1 {
		value, evaluateErr := card.Evaluate(`el => { const values = [...el.querySelectorAll('*')].filter(x => x.children.length === 0 && /^₹[\d,.]+$/.test(x.textContent.trim()) && (() => { for (let a=x; a && a!==el; a=a.parentElement) { if (getComputedStyle(a).textDecorationLine.includes('line-through')) return true; } return false; })()).map(x => x.textContent.trim()); return [...new Set(values)].length === 1 ? values[0] : ''; }`, nil)
		if evaluateErr == nil {
			state.OriginalPrice, _ = value.(string)
		}
	}
	state.Teachers, _ = page.Locator(`#teachers [aria-roledescription="slide"]`).AllInnerTexts()
	return state, nil
}

// Manual traces are opt-in and retain at most five archives.
func prepareTraces(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(dir, "pw-*.zip"))
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		left, leftErr := os.Stat(entries[i])
		if leftErr != nil {
			return false
		}
		right, rightErr := os.Stat(entries[j])
		return rightErr == nil && left.ModTime().Before(right.ModTime())
	})
	for len(entries) >= 5 {
		if err := os.Remove(entries[0]); err != nil {
			return err
		}
		entries = entries[1:]
	}
	return nil
}
