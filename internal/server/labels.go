package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Labels are key=value pairs: a daemon's owner-set labels and reported
// facts, and the labels a task requires of its daemon
// (docs/adr/2026-10-09-agents-and-placement.md).
type Labels map[string]string

const (
	maxLabelKey   = 63
	maxLabelValue = 255
)

var errInvalidLabels = errors.New("invalid labels")

// validLabelKey reports whether key is 1 to 63 ASCII letters, digits,
// '.', '_' and '-'.
func validLabelKey(key string) bool {
	if key == "" || len(key) > maxLabelKey {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// validLabelValue reports whether value is 1 to 255 printable ASCII
// characters other than space, ',' and '=', which separate labels when
// they are written as text.
func validLabelValue(value string) bool {
	if value == "" || len(value) > maxLabelValue {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r > '~' || r == ',' || r == '=' {
			return false
		}
	}
	return true
}

// Validate reports the first key or value, in key order, that does not
// follow the rules for labels.
func (l Labels) Validate() error {
	for _, key := range slices.Sorted(maps.Keys(l)) {
		if !validLabelKey(key) {
			return fmt.Errorf("%w: key %q must be 1 to %d letters, digits, '.', '_' or '-'", errInvalidLabels, key, maxLabelKey)
		}
		if !validLabelValue(l[key]) {
			return fmt.Errorf("%w: value %q of %s must be 1 to %d printable characters other than space, ',' and '='", errInvalidLabels, l[key], key, maxLabelValue)
		}
	}
	return nil
}

// ParseLabels reads labels written as key=value pairs separated by
// commas, spaces or line breaks, as the GUI's fields take them. A key
// given twice is refused.
func ParseLabels(text string) (Labels, error) {
	labels := Labels{}
	for _, pair := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not key=value", errInvalidLabels, pair)
		}
		if _, twice := labels[key]; twice {
			return nil, fmt.Errorf("%w: %s is given twice", errInvalidLabels, key)
		}
		labels[key] = value
	}
	return labels, labels.Validate()
}

// String writes the labels as ParseLabels reads them, in key order.
func (l Labels) String() string {
	pairs := make([]string, 0, len(l))
	for _, key := range slices.Sorted(maps.Keys(l)) {
		pairs = append(pairs, key+"="+l[key])
	}
	return strings.Join(pairs, ", ")
}

// Holds reports whether l has every key=value of required.
func (l Labels) Holds(required Labels) bool {
	for key, value := range required {
		if l[key] != value {
			return false
		}
	}
	return true
}

// Merge returns facts with labels over them: an owner's label wins over
// a fact with the same key.
func Merge(facts, labels Labels) Labels {
	merged := maps.Clone(facts)
	if merged == nil {
		merged = Labels{}
	}
	maps.Copy(merged, labels)
	return merged
}

// encodeLabels is l as stored: a JSON object, {} when l is empty.
func encodeLabels(l Labels) string {
	if len(l) == 0 {
		return "{}"
	}
	data, err := json.Marshal(l)
	if err != nil {
		// A map of strings always encodes.
		panic(err)
	}
	return string(data)
}

// decodeLabels reads labels as stored.
func decodeLabels(raw string) (Labels, error) {
	labels := Labels{}
	if raw == "" {
		return labels, nil
	}
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}
