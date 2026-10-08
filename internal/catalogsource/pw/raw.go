package pw

import (
	"net/url"
	"strings"
)

func catalogSourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// safeCatalogValue retains the matched public batch structure for local
// analysis while excluding account, contact, credential and video fields.
// The whole page/response is never written to the raw report.
func safeCatalogValue(value any) any {
	switch v := value.(type) {
	case object:
		return safeCatalogMap(v)
	case map[string]any:
		return safeCatalogMap(v)
	case []any:
		out := make([]any, len(v))
		for index, child := range v {
			out[index] = safeCatalogValue(child)
		}
		return out
	case string:
		if len(v) > 8192 {
			return "[omitted: long text]"
		}
		return v
	default:
		return v
	}
}

func safeCatalogMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, child := range value {
		if rawFieldExcluded(key) {
			out[key] = "[redacted]"
			continue
		}
		out[key] = safeCatalogValue(child)
	}
	return out
}

func rawFieldExcluded(key string) bool {
	key = strings.ToLower(key)
	for _, part := range []string{
		"token", "secret", "password", "cookie", "authorization", "credential",
		"session", "email", "phone", "mobile", "contact", "address",
		"account", "profile", "video", "recording", "stream",
	} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}
