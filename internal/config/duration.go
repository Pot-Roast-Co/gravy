package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads and writes as a Go duration string ("30m", "1h30m")
// rather than as a nanosecond count, so the config file stays legible and hand-editable.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration as it is written in the file.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string, reporting the offending line on failure.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: expected a duration string such as \"30m\": %w", n.Line, err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (try \"30s\", \"10m\", \"1h30m\")", n.Line, s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the duration back as a string, so a Load/Save round trip is stable.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
