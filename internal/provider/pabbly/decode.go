package pabbly

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Pabbly wraps most responses in {"status":..,"data":{..}} but the inner collection
// key differs per endpoint. Rather than guess a rigid schema we decode into generic
// maps and read the first key that is actually present. Verify against a real
// account response before relying on any single key here.
type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// unwrap returns the payload body, tolerating both enveloped and bare responses.
func unwrap(raw json.RawMessage) json.RawMessage {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		return env.Data
	}
	return raw
}

type object map[string]any

// asObject coerces a decoded value into an object.
func asObject(v any) (object, bool) {
	m, ok := v.(map[string]any)
	return object(m), ok
}

func toObjects(in []map[string]any) []object {
	out := make([]object, len(in))
	for i, m := range in {
		out[i] = object(m)
	}
	return out
}

// collection pulls a list out of a payload that may itself be the list, or may hold
// it under one of the candidate keys.
func collection(raw json.RawMessage, keys ...string) []object {
	body := unwrap(raw)

	var direct []map[string]any
	if err := json.Unmarshal(body, &direct); err == nil {
		return toObjects(direct)
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if list, ok := v.([]any); ok {
				out := make([]object, 0, len(list))
				for _, item := range list {
					if o, ok := asObject(item); ok {
						out = append(out, o)
					}
				}
				return out
			}
		}
	}
	return nil
}

// single pulls one object out of a payload, optionally descending into a wrapper key.
func single(raw json.RawMessage, keys ...string) object {
	body := unwrap(raw)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if o, ok := asObject(v); ok {
				return o
			}
			// Some endpoints wrap the single record in a one-element list.
			if list, ok := v.([]any); ok && len(list) > 0 {
				if o, ok := asObject(list[0]); ok {
					return o
				}
			}
		}
	}
	return object(m)
}

// str reads the first present non-empty string-ish value among keys.
func (o object) str(keys ...string) string {
	for _, k := range keys {
		v, ok := o[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				return s
			}
		case json.Number:
			return t.String()
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		}
	}
	return ""
}

// raw returns the first present value among keys, for amount parsing.
func (o object) raw(keys ...string) any {
	for _, k := range keys {
		if v, ok := o[k]; ok && v != nil {
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				continue
			}
			return v
		}
	}
	return nil
}

func (o object) nested(keys ...string) object {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			if m, ok := asObject(v); ok {
				return m
			}
			// Metadata is sometimes delivered as a JSON-encoded string.
			if s, ok := v.(string); ok && s != "" {
				var m map[string]any
				if json.Unmarshal([]byte(s), &m) == nil {
					return object(m)
				}
			}
		}
	}
	return nil
}

// boolish treats Pabbly's mixed true/"1"/"active" representations as one concept.
func (o object) boolish(def bool, keys ...string) bool {
	for _, k := range keys {
		v, ok := o[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case float64:
			return t != 0
		case json.Number:
			n, _ := t.Int64()
			return n != 0
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "1", "true", "active", "yes", "enabled":
				return true
			case "0", "false", "inactive", "no", "disabled":
				return false
			}
		}
	}
	return def
}

func (o object) intPtr(keys ...string) *int32 {
	s := o.str(keys...)
	if s == "" {
		return nil
	}
	// Tolerate values like "Class 12".
	s = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(s), "class"))
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return nil
	}
	v := int32(n)
	return &v
}

var timeLayouts = []string{
	time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05", "2006-01-02",
}

func (o object) timePtr(keys ...string) *time.Time {
	s := o.str(keys...)
	if s == "" {
		return nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}
