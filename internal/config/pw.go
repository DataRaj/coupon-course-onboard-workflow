package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type PWConfig struct {
	ListingURL        string
	BatchSlugs        map[string]struct{}
	MaxBatches        int
	Timeout           time.Duration
	NavigationTimeout time.Duration
	Headless          bool
	Trace             bool
	TraceDir          string
	ExtractorVersion  string
}

func LoadPW() (PWConfig, error) {
	maxBatches := 300
	if raw := env("PW_MAX_BATCHES", ""); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return PWConfig{}, fmt.Errorf("PW_MAX_BATCHES must be an integer")
		}
		maxBatches = value
	}
	c := PWConfig{
		ListingURL:        env("PW_LISTING_URL", "https://www.pw.live/iit-jee/batches"),
		BatchSlugs:        map[string]struct{}{},
		MaxBatches:        maxBatches,
		Timeout:           dur("PW_BROWSER_TIMEOUT", 5*time.Minute),
		NavigationTimeout: dur("PW_NAVIGATION_TIMEOUT", 45*time.Second),
		Headless:          env("PW_HEADLESS", "true") == "true",
		Trace:             env("PW_TRACE", "false") == "true",
		TraceDir:          env("PW_TRACE_DIR", "logs/pw-traces"),
		ExtractorVersion:  "pw-extractor-v2",
	}
	u, err := url.Parse(c.ListingURL)
	if err != nil || u.Scheme != "https" || u.Host != "www.pw.live" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/batches") {
		return c, fmt.Errorf("PW_LISTING_URL must be a public www.pw.live batches URL without query or fragment")
	}
	c.ListingURL = strings.TrimRight(u.String(), "/")
	for _, slug := range strings.Split(env("PW_BATCH_SLUGS", ""), ",") {
		if slug = strings.TrimSpace(slug); slug != "" {
			if strings.ContainsAny(slug, "/?#%") {
				return c, fmt.Errorf("PW_BATCH_SLUGS contains an invalid slug")
			}
			c.BatchSlugs[slug] = struct{}{}
		}
	}
	if c.MaxBatches < 1 || c.MaxBatches > 500 {
		return c, fmt.Errorf("PW_MAX_BATCHES must be between 1 and 500")
	}
	if c.Timeout <= 0 || c.Timeout > 15*time.Minute || c.NavigationTimeout <= 0 || c.NavigationTimeout > c.Timeout {
		return c, fmt.Errorf("PW timeouts must be positive, navigation <= browser <= 15m")
	}
	return c, nil
}
