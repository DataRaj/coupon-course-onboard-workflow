package pw

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxPayloadBytes = 4 << 20

type Payload struct {
	Body   []byte
	Source string
	URL    string
}

// DOMState contains only selected public semantic sections, never the whole page.
type DOMState struct {
	Title         string   `json:"title"`
	Thumbnail     string   `json:"thumbnail"`
	BasePlan      string   `json:"base_plan"`
	PurchaseCard  string   `json:"purchase_card"`
	OriginalPrice string   `json:"original_price"`
	About         string   `json:"about"`
	Teachers      []string `json:"teachers"`
}

// RawCapture keeps the public batch-shaped input used during extraction. It is
// written to a separate local report, never included in the normalized report.
type RawCapture struct {
	SelectedSource string           `json:"selected_source,omitempty"`
	Sources        []SourceSnapshot `json:"sources"`
	RenderedDOM    DOMState         `json:"rendered_dom"`
}

type SourceSnapshot struct {
	Source    string `json:"source"`
	SourceURL string `json:"source_url,omitempty"`
	Index     int    `json:"index"`
	Selected  bool   `json:"selected"`
	Batch     any    `json:"batch"`
	Plans     any    `json:"plans,omitempty"`
}

type object map[string]any

func text(v any) string {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "$") {
			return ""
		}
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	}
	return ""
}
func obj(v any) object {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
func array(v any) []any { a, _ := v.([]any); return a }
func decodeJSON(b []byte) (any, error) {
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("payload exceeds limit")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return v, nil
}

// flightObjects reads public React flight records without executing page scripts.
// Text records use byte lengths and may contain newlines; skip them correctly.
// Only JSON records are considered, and only a matched batch object is extracted.
func flightObjects(raw string) []any {
	if len(raw) > maxPayloadBytes {
		return nil
	}
	var result []any
	for len(raw) > 0 {
		raw = strings.TrimLeft(raw, "\r\n")
		colon := strings.IndexByte(raw, ':')
		if colon < 1 {
			break
		}
		if _, err := strconv.ParseUint(raw[:colon], 16, 64); err != nil {
			break
		}
		raw = raw[colon+1:]
		if strings.HasPrefix(raw, "T") {
			comma := strings.IndexByte(raw, ',')
			if comma < 2 {
				break
			}
			n, err := strconv.ParseUint(raw[1:comma], 16, 32)
			if err != nil || int(n) > len(raw)-comma-1 {
				break
			}
			raw = raw[comma+1+int(n):]
			continue
		}
		line, rest, ok := strings.Cut(raw, "\n")
		if !ok {
			rest = ""
		}
		if v, err := decodeJSON([]byte(line)); err == nil {
			result = append(result, v)
		}
		raw = rest
	}
	return result
}

// findBatch never accepts a price from an unrelated recommendation or variant.
func findBatch(v any, slug string, depth int) (object, []any) {
	if depth > 32 {
		return nil, nil
	}
	switch x := v.(type) {
	case map[string]any:
		if text(x["slug"]) == slug && text(x["_id"]) != "" && text(x["name"]) != "" {
			return x, nil
		}
		// The public detail component carries description + sibling batchPlans.
		if d := obj(x["description"]); d != nil && text(d["slug"]) == slug && text(d["_id"]) != "" {
			return d, array(x["batchPlans"])
		}
		for _, child := range x {
			if b, plans := findBatch(child, slug, depth+1); b != nil {
				return b, plans
			}
		}
	case []any:
		for _, child := range x {
			if b, plans := findBatch(child, slug, depth+1); b != nil {
				return b, plans
			}
		}
	}
	return nil, nil
}

func Extract(canonical string, acquired time.Time, network []Payload, embedded []string, flight string, dom DOMState) (PWBatchDTO, error) {
	d, _, err := extractWithRaw(canonical, acquired, network, embedded, flight, dom)
	return d, err
}

func extractWithRaw(canonical string, acquired time.Time, network []Payload, embedded []string, flight string, dom DOMState) (PWBatchDTO, RawCapture, error) {
	raw := RawCapture{Sources: []SourceSnapshot{}, RenderedDOM: dom}
	slug, canonical, err := targetIdentity(canonical)
	if err != nil {
		return PWBatchDTO{}, raw, err
	}
	d := PWBatchDTO{Provider: Provider, Slug: slug, CanonicalURL: canonical, AcquiredAt: acquired, Provenance: map[string]string{}, Availability: "unknown"}
	var batch object
	var plans []any
	source := ""
	seenSnapshots := map[string]struct{}{}
	collect := func(kind, sourceURL string, index int, candidate object, candidatePlans []any) {
		if candidate == nil {
			return
		}
		safeBatch, safePlans := safeCatalogValue(candidate), safeCatalogValue(candidatePlans)
		encoded, err := json.Marshal([]any{safeBatch, safePlans})
		if err != nil {
			return
		}
		digest := sha256.Sum256(encoded)
		fingerprint := fmt.Sprintf("%s:%x", kind, digest)
		if _, duplicate := seenSnapshots[fingerprint]; duplicate {
			return
		}
		seenSnapshots[fingerprint] = struct{}{}
		selected := batch == nil
		raw.Sources = append(raw.Sources, SourceSnapshot{
			Source: kind, SourceURL: catalogSourceURL(sourceURL), Index: index, Selected: selected,
			Batch: safeBatch, Plans: safePlans,
		})
		if selected {
			batch, plans = candidate, candidatePlans
			raw.SelectedSource = kind
			switch kind {
			case "json_script", "react_flight":
				source = "embedded_state"
			default:
				source = kind
			}
		}
	}
	for index, p := range network {
		if v, e := decodeJSON(p.Body); e == nil {
			candidate, candidatePlans := findBatch(v, slug, 0)
			collect("network_json", p.URL, index, candidate, candidatePlans)
		}
	}
	for index, script := range embedded {
		if v, e := decodeJSON([]byte(script)); e == nil {
			candidate, candidatePlans := findBatch(v, slug, 0)
			collect("json_script", "", index, candidate, candidatePlans)
		}
	}
	for index, v := range flightObjects(flight) {
		candidate, candidatePlans := findBatch(v, slug, 0)
		collect("react_flight", "", index, candidate, candidatePlans)
	}
	if batch != nil {
		extractBatch(&d, batch, plans, source)
	}
	applyDOM(&d, dom)
	if raw.SelectedSource == "" && d.Provenance["title"] == "rendered_dom" {
		raw.SelectedSource = "rendered_dom"
	}
	if d.Title == "" {
		return d, raw, failure("identity_missing", fmt.Errorf("batch title not found in structured or rendered state"))
	}
	if dom.Title != "" && d.Title != strings.TrimSpace(dom.Title) {
		return d, raw, failure("identity_mismatch", fmt.Errorf("structured and rendered batch titles differ"))
	}
	// Category/class may be taken from a validated canonical hierarchy, never
	// guessed from promotional title text.
	if d.Class == nil {
		var class int32
		switch {
		case strings.Contains(canonical, "/class-11/"):
			class = 11
		case strings.Contains(canonical, "/class-12/"):
			class = 12
		}
		if class != 0 {
			d.Class = &class
			d.Provenance["class"] = "canonical_path"
		}
	}
	if d.TargetExam == "" && strings.Contains(canonical, "/iit-jee/") {
		d.TargetExam = "IIT-JEE"
		d.Provenance["target_exam"] = "canonical_path"
	}
	return d, raw, nil
}

func extractBatch(d *PWBatchDTO, b object, plans []any, source string) {
	set := func(key string, dest *string, value string) {
		if value != "" {
			*dest = value
			d.Provenance[key] = source
		}
	}
	set("external_id", &d.ExternalID, text(b["_id"]))
	set("title", &d.Title, text(b["name"]))
	set("language", &d.Language, text(b["language"]))
	set("mode", &d.Mode, text(b["mode"]))
	if v, e := strconv.ParseInt(text(b["class"]), 10, 32); e == nil {
		c := int32(v)
		d.Class = &c
		d.Provenance["class"] = source
	}
	if y, e := strconv.Atoi(text(b["examYear"])); e == nil && y >= 2000 && y <= 2200 {
		d.TargetYear = &y
		d.Provenance["target_year"] = source
	}
	for _, e := range array(b["exam"]) {
		if text(e) == "IIT-JEE" {
			set("target_exam", &d.TargetExam, "IIT-JEE")
		}
	}
	for _, date := range []struct {
		key  string
		dest **time.Time
	}{{"startDate", &d.StartDate}, {"endDate", &d.EndDate}} {
		raw := text(b[date.key])
		if raw != "" {
			*date.dest = parseDate(raw)
			if *date.dest == nil {
				d.Warnings = append(d.Warnings, date.key+"_invalid")
			} else {
				d.Provenance[date.key] = source
			}
		}
	}
	if text(b["status"]) == "Active" {
		set("availability", &d.Availability, "available")
	}
	if image := publicStructuredImage(b["previewImage"]); image != "" {
		set("thumbnail", &d.Thumbnail, image)
	}
	fee := obj(b["fee"])
	// fee.total is the full base-plan selling amount, not iOS purchase pricing.
	if fee != nil && text(b["priceLabel"]) == "(For Full Batch)" && text(fee["name"]) == "Batch" {
		set("selling_price", &d.SellingPrice, text(fee["total"]))
		if d.SellingPrice != "" {
			d.SellingPriceContext = "selling_price"
		}
		if text(fee["currency"]) == "INR" {
			set("currency", &d.Currency, "INR")
		}
		if original := text(fee["price"]); original != "" {
			set("original_price", &d.OriginalPrice, original)
		}
		if discount := text(fee["discount"]); discount != "" {
			set("discount", &d.Discount, discount)
		}
	}
	for _, sub := range array(b["subjects"]) {
		s := obj(sub)
		if s == nil {
			continue
		}
		resources, _ := s["isResources"].(bool)
		subject := text(s["subject"])
		if resources || subject == "" || subject == "Notices" {
			continue
		}
		d.Subjects = append(d.Subjects, subject)
		for _, teacher := range array(s["teacherIds"]) {
			t := obj(teacher)
			if t == nil {
				continue
			}
			name := strings.TrimSpace(text(t["firstName"]) + " " + text(t["lastName"]))
			if name != "" {
				d.Faculty = append(d.Faculty, Faculty{Name: name, Subject: subject})
			}
		}
	}
	if len(d.Subjects) > 0 {
		d.Provenance["subjects"] = source
	}
	if len(d.Faculty) > 0 {
		d.Provenance["faculty"] = source
	}
	for _, plan := range plans {
		p := obj(plan)
		name := text(p["title"])
		if name == "" {
			continue
		}
		entry := PWPlanDTO{Name: name, SellingPrice: text(p["total"]), OriginalPrice: text(p["price"])}
		for _, item := range array(p["tableItems"]) {
			i := obj(item)
			enabled, _ := i["enabled"].(bool)
			if enabled && text(i["item"]) != "" {
				entry.Features = append(entry.Features, text(i["item"]))
			}
		}
		d.Plans = append(d.Plans, entry)
		if name == "Batch" {
			d.Features = append(d.Features, entry.Features...)
		}
	}
	if len(d.Plans) > 0 {
		d.Provenance["plans"] = source
	}
	if len(d.Features) > 0 {
		d.Provenance["features"] = source
	}
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
func basePrice(s string) string {
	ls := lines(s)
	// This section is scoped to #features and its exact base-plan label.
	for i, l := range ls {
		if l == "Batch" && i+1 < len(ls) && strings.HasPrefix(ls[i+1], "₹") {
			return ls[i+1]
		}
	}
	return ""
}

func domPlans(raw string) []PWPlanDTO {
	var plans []PWPlanDTO
	var current *PWPlanDTO
	for _, line := range lines(raw) {
		if line == "Batch" || line == "Infinity" {
			plans = append(plans, PWPlanDTO{Name: line})
			current = &plans[len(plans)-1]
			continue
		}
		if current == nil {
			continue
		}
		if line == "Select" {
			current = nil
			continue
		}
		if current.SellingPrice == "" && strings.HasPrefix(line, "₹") {
			if _, err := ParsePrice(line); err == nil {
				current.SellingPrice = line
			}
			continue
		}
		current.Features = append(current.Features, line)
	}
	return plans
}

func applyDOM(d *PWBatchDTO, dom DOMState) {
	if d.Description == "" && strings.TrimSpace(dom.About) != "" {
		d.Description = strings.TrimSpace(dom.About)
		d.Provenance["description"] = "rendered_dom"
	}

	set := func(key string, dest *string, v string) {
		if *dest == "" && v != "" {
			*dest = v
			d.Provenance[key] = "rendered_dom"
		}
	}
	set("title", &d.Title, strings.TrimSpace(dom.Title))
	if d.Thumbnail == "" && publicImageURL(dom.Thumbnail) {
		set("thumbnail", &d.Thumbnail, strings.TrimSpace(dom.Thumbnail))
	}
	display := basePrice(dom.BasePlan)
	if display != "" {
		if _, e := ParsePrice(display); e == nil {
			set("currency", &d.Currency, "INR")
			set("selling_price", &d.SellingPrice, display)
			d.SellingPriceContext = "selling_price"
			if a, e := ParsePrice(d.SellingPrice); e == nil {
				b, _ := ParsePrice(display)
				if a != b {
					// PW renders whole rupees but its state may contain paise. Preserve the
					// precise amount only when the discrepancy is less than one rupee.
					diff := a - b
					if diff < 0 {
						diff = -diff
					}
					if diff >= 100 {
						d.SellingPrice = ""
						d.SellingPriceContext = ""
						d.Warnings = append(d.Warnings, "selling_price_conflict: structured and base-plan DOM disagree")
					} else {
						d.Warnings = append(d.Warnings, "selling_price_display_rounded: retaining structured paise")
					}
				}
			}
		}
	}
	if dom.OriginalPrice != "" {
		set("original_price", &d.OriginalPrice, dom.OriginalPrice)
	}
	if len(d.Plans) == 0 {
		d.Plans = domPlans(dom.BasePlan)
		if len(d.Plans) > 0 {
			d.Provenance["plans"] = "rendered_dom"
		}
	}
	for _, l := range lines(dom.PurchaseCard) {
		if l == "English" || l == "Hindi" || l == "Hinglish" {
			set("language", &d.Language, l)
		}
		if strings.HasPrefix(l, "Starts on ") && d.StartDate == nil {
			d.StartDate = parseDate(strings.TrimPrefix(l, "Starts on "))
			if d.StartDate != nil {
				d.Provenance["startDate"] = "rendered_dom"
			}
		}
		if strings.HasPrefix(l, "Ends on ") && d.EndDate == nil {
			d.EndDate = parseDate(strings.TrimPrefix(l, "Ends on "))
			if d.EndDate != nil {
				d.Provenance["endDate"] = "rendered_dom"
			}
		}
	}
	if len(d.Subjects) == 0 {
		for _, l := range lines(dom.About) {
			if strings.HasPrefix(l, "Subjects:") {
				s := strings.TrimSpace(strings.TrimPrefix(l, "Subjects:"))
				s = strings.ReplaceAll(s, " and ", ",")
				for _, part := range strings.Split(s, ",") {
					if part = strings.TrimSpace(part); part != "" {
						d.Subjects = append(d.Subjects, part)
					}
				}
				d.Provenance["subjects"] = "rendered_dom"
			}
		}
	}
	if d.Schedule == "" {
		about := lines(dom.About)
		for index, line := range about {
			if !strings.HasPrefix(line, "Schedule:") {
				continue
			}
			d.Schedule = strings.TrimSpace(strings.TrimPrefix(line, "Schedule:"))
			if d.Schedule == "" && index+1 < len(about) {
				d.Schedule = about[index+1]
			}
			if d.Schedule != "" {
				d.Provenance["schedule"] = "rendered_dom"
			}
			break
		}
	}
	if len(d.Faculty) == 0 {
		for _, card := range dom.Teachers {
			ls := lines(card)
			if len(ls) >= 2 && ls[0] != "" {
				faculty := Faculty{Name: ls[0], Subject: ls[1]}
				if len(ls) >= 3 {
					faculty.Experience = ls[2]
				}
				d.Faculty = append(d.Faculty, faculty)
			}
		}
		if len(d.Faculty) > 0 {
			d.Provenance["faculty"] = "rendered_dom"
		}
	}
	if len(d.Features) == 0 {
		ls := lines(dom.BasePlan)
		active := false
		for _, l := range ls {
			if l == "Batch" {
				active = true
				continue
			}
			if active {
				if l == "Select" || l == "Infinity" {
					break
				}
				if !strings.HasPrefix(l, "₹") {
					d.Features = append(d.Features, l)
				}
			}
		}
		if len(d.Features) > 0 {
			d.Provenance["features"] = "rendered_dom"
		}
	}
}

func publicImageURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && u.Hostname() == "static.pw.live" && u.User == nil
}

func publicStructuredImage(value any) string {
	if direct := text(value); publicImageURL(direct) {
		return direct
	}
	image := obj(value)
	if image == nil {
		return ""
	}
	base, key := strings.TrimRight(text(image["baseUrl"]), "/"), strings.TrimLeft(text(image["key"]), "/")
	candidate := base + "/" + key
	if base == "" || key == "" || !publicImageURL(candidate) {
		return ""
	}
	return candidate
}

// DiscoverTargets extracts bounded public detail targets. Structured payloads
// are considered before rendered links, while the final result is deduplicated
// by canonical URL and remains in discovery order.
func DiscoverTargets(listingURL string, payloads []Payload, embedded []string, flight string, hrefs []string, max int, selected map[string]struct{}) []Target {
	seen := map[string]struct{}{}
	result := make([]Target, 0, max)
	add := func(raw, title string) {
		if len(result) >= max {
			return
		}
		canonical, err := canonicalTargetURL(listingURL, raw)
		if err != nil {
			return
		}
		slug, canonical, err := targetIdentity(canonical)
		if err != nil {
			return
		}
		if len(selected) > 0 {
			if _, ok := selected[slug]; !ok {
				return
			}
		}
		if _, ok := seen[canonical]; ok {
			return
		}
		seen[canonical] = struct{}{}
		result = append(result, Target{Slug: slug, CanonicalURL: canonical, Title: strings.TrimSpace(title)})
	}
	for _, payload := range payloads {
		if value, err := decodeJSON(payload.Body); err == nil {
			walkCandidateData(value, 0, add)
		}
	}
	for _, raw := range embedded {
		if value, err := decodeJSON([]byte(raw)); err == nil {
			walkCandidateData(value, 0, add)
		}
	}
	for _, value := range flightObjects(flight) {
		walkCandidateData(value, 0, add)
	}
	for _, href := range hrefs {
		add(href, "")
	}
	return result
}

func canonicalTargetURL(listingURL, raw string) (string, error) {
	base, err := url.Parse(listingURL)
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "/") {
		raw = strings.TrimRight(listingURL, "/") + "/" + raw
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(reference)
	resolved.RawQuery = ""
	resolved.Fragment = ""
	canonical := resolved.String()
	catalogPrefix := strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/batches") + "/"
	if !strings.HasPrefix(resolved.Path, catalogPrefix) {
		return "", fmt.Errorf("target is outside configured listing")
	}
	_, canonical, err = targetIdentity(canonical)
	return canonical, err
}

func walkCandidateData(value any, depth int, add func(string, string)) {
	if depth > 24 {
		return
	}
	switch current := value.(type) {
	case map[string]any:
		title := text(current["name"])
		if title == "" {
			title = text(current["title"])
		}
		for _, key := range []string{"url", "href", "link", "route"} {
			if candidate := text(current[key]); strings.Contains(candidate, "/batches/") {
				add(candidate, title)
			}
		}
		if slug := text(current["slug"]); title != "" && text(current["_id"]) != "" && slug != "" && !strings.ContainsAny(slug, "/?#%") {
			add(slug, title)
		}
		for _, child := range current {
			walkCandidateData(child, depth+1, add)
		}
	case []any:
		for _, child := range current {
			walkCandidateData(child, depth+1, add)
		}
	}
}
