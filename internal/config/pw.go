package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type PWConfig struct {
	ListingURL        string
	BatchURL          string
	ExpectedTitle     string
	Timeout           time.Duration
	NavigationTimeout time.Duration
	Headless          bool
	Trace             bool
	TraceDir          string
	ExtractorVersion  string
}

func LoadPW() (PWConfig, error) {
	c := PWConfig{
		ListingURL:        env("PW_LISTING_URL", "https://www.pw.live/iit-jee/class-12/batches"),
		BatchURL:          env("PW_BATCH_URL", "https://www.pw.live/iit-jee/class-12/batches/lakshya-jee-in-english-2027-348091"),
		ExpectedTitle:     env("PW_EXPECTED_TITLE", ""),
		Timeout:           dur("PW_BROWSER_TIMEOUT", 90*time.Second),
		NavigationTimeout: dur("PW_NAVIGATION_TIMEOUT", 45*time.Second),
		Headless:          env("PW_HEADLESS", "true") == "true",
		Trace:             env("PW_TRACE", "false") == "true",
		TraceDir:          env("PW_TRACE_DIR", "logs/pw-traces"),
		ExtractorVersion:  "pw-extractor-v1",
	}
	for _, raw := range []string{c.ListingURL, c.BatchURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host != "www.pw.live" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/iit-jee/class-12/batches") {
			return c, fmt.Errorf("PW target must be a public www.pw.live IIT-JEE Class 12 batch URL without query or fragment")
		}
	}
	if !strings.HasPrefix(c.BatchURL, strings.TrimRight(c.ListingURL, "/")+"/") {
		return c, fmt.Errorf("PW_BATCH_URL must be a detail under PW_LISTING_URL")
	}
	if c.Timeout <= 0 || c.Timeout > 5*time.Minute || c.NavigationTimeout <= 0 || c.NavigationTimeout > c.Timeout {
		return c, fmt.Errorf("PW timeouts must be positive, navigation <= browser <= 5m")
	}
	return c, nil
}

// LoadPWCommand keeps the manual catalog ingestion independent of Pabbly
// credentials and JWT configuration used by the long-running API and worker.
func LoadPWCommand() (PWConfig, string, error) {
	c, err := LoadPW()
	if err != nil {
		return c, "", err
	}
	databaseURL := env("DATABASE_URL", "")
	if databaseURL == "" {
		return c, "", fmt.Errorf("DATABASE_URL is required for PW ingestion")
	}
	return c, databaseURL, nil
}
