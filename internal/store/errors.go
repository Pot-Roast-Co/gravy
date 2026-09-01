package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// notFound wraps ErrNotFound with what was being looked for.
func notFound(kind, id string) error {
	return fmt.Errorf("%s %q: %w", kind, id, ErrNotFound)
}

// marshalJSON encodes a value for a JSON-typed column.
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode json column: %w", err)
	}
	return string(b), nil
}

// unmarshalJSON decodes a JSON-typed column, tolerating empty values so a row written before a
// column existed still reads.
func unmarshalJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("decode json column: %w", err)
	}
	return nil
}

// unixOrZero converts a timestamp to seconds, mapping the zero time to 0.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// timeOrZero converts seconds back to a time, mapping 0 to the zero time.
func timeOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
