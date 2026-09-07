package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
	"gopkg.in/yaml.v3"
)

// ErrInvalid is returned by Validate for any configuration problem.
var ErrInvalid = errors.New("invalid configuration")

// Error is one configuration problem, located precisely enough to fix without hunting.
type Error struct {
	// Key is the dotted path of the offending setting, for example "routes.implementation[1]".
	Key string
	// Line is the 1-based line in the file, or 0 when it could not be located.
	Line int
	// Msg says what is wrong and, where possible, what would be right.
	Msg  string
	File string
}

func (e *Error) Error() string {
	loc := e.File
	if loc == "" {
		loc = "config"
	}
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
	}
	return fmt.Sprintf("%s: %s: %s", loc, e.Key, e.Msg)
}

// Unwrap allows errors.Is(err, ErrInvalid).
func (e *Error) Unwrap() error { return ErrInvalid }

// Errors is a set of configuration problems, reported together so that fixing the file is one
// pass rather than a guessing game.
type Errors []*Error

func (es Errors) Error() string {
	if len(es) == 1 {
		return es[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration problems:", len(es))
	for _, e := range es {
		b.WriteString("\n  ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// Unwrap allows errors.Is(err, ErrInvalid).
func (es Errors) Unwrap() error { return ErrInvalid }

// locator resolves a dotted key path to a line number in the source document. It is nil when
// the config did not come from a file, in which case errors carry no line.
type locator struct {
	doc  *yaml.Node
	file string
}

// line returns the line of the given key path, or 0 if it cannot be found. Path elements are
// mapping keys; a "name[i]" element indexes into a sequence.
//
// For a plain key the line reported is the key's own, not its value's: when the complaint is
// "this setting is wrong", the key is where the reader needs to look. For an indexed element it
// is the element's line, which is the offending entry itself.
func (l *locator) line(path string) int {
	if l == nil || l.doc == nil {
		return 0
	}
	n := l.doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := 0
	for _, part := range strings.Split(path, ".") {
		key, idx, hasIdx := splitIndex(part)
		if n.Kind != yaml.MappingNode {
			return 0
		}
		found := false
		// Mapping content alternates key, value.
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != key {
				continue
			}
			line = n.Content[i].Line
			value := n.Content[i+1]
			if hasIdx {
				if value.Kind != yaml.SequenceNode || idx >= len(value.Content) {
					// The sequence is missing or too short; the key is the best anchor.
					return line
				}
				n = value.Content[idx]
				line = n.Line
			} else {
				n = value
			}
			found = true
			break
		}
		if !found {
			return 0
		}
	}
	return line
}

// splitIndex parses "name[3]" into ("name", 3, true).
func splitIndex(part string) (string, int, bool) {
	open := strings.IndexByte(part, '[')
	if open < 0 || !strings.HasSuffix(part, "]") {
		return part, 0, false
	}
	var idx int
	if _, err := fmt.Sscanf(part[open+1:len(part)-1], "%d", &idx); err != nil {
		return part, 0, false
	}
	return part[:open], idx, true
}

// Validate checks a configuration and reports every problem it finds, each naming the offending
// key and, when the config came from a file, its line.
func (c Config) Validate() error { return c.validate(nil) }

func (c Config) validate(loc *locator) error {
	var errs Errors
	add := func(key, format string, args ...any) {
		e := &Error{Key: key, Msg: fmt.Sprintf(format, args...)}
		if loc != nil {
			e.Line, e.File = loc.line(key), loc.file
		}
		errs = append(errs, e)
	}

	if c.Concurrency.Workers < 1 {
		add("concurrency.workers", "must be at least 1, got %d", c.Concurrency.Workers)
	}

	for _, id := range sortedKeys(c.Providers) {
		if c.Providers[id].Command == "" {
			add("providers."+id+".command", "must name an executable, for example \"claude\"")
		}
	}

	// Routes: the key must be a known route, and every choice must name a configured provider.
	// An unknown route is a typo that would otherwise fail silently at scheduling time.
	for _, r := range sortedRoutes(c.Routes) {
		// A route is whatever you name it; only names that would be ambiguous are refused.
		if !r.Named() {
			add("routes."+string(r), "not a usable bucket name: use a word without spaces, "+
				"slashes, commas or colons")
			continue
		}
		for i, entry := range c.Routes[r] {
			key := fmt.Sprintf("routes.%s[%d]", r, i)
			ch, err := ParseChoice(entry)
			if err != nil {
				add(key, "%v, for example \"claude-code/sonnet\"", err)
				continue
			}
			if _, ok := c.Providers[ch.ProviderID]; !ok {
				add(key, "references provider %q, which is not configured (configured: %s)",
					ch.ProviderID, strings.Join(sortedKeys(c.Providers), ", "))
			}
		}
	}

	if c.Timeouts.Run <= 0 {
		add("timeouts.run", "must be positive, got %s", c.Timeouts.Run)
	}
	if c.Timeouts.ValidationStep <= 0 {
		add("timeouts.validation_step", "must be positive, got %s", c.Timeouts.ValidationStep)
	}
	if c.Timeouts.Stall < 0 {
		add("timeouts.stall", "must not be negative, got %s (use 0 to disable stall detection)", c.Timeouts.Stall)
	}
	// A stall timeout at or above the run timeout can never fire, which would quietly disable
	// the only detector that distinguishes a wedged run from a slow one.
	if c.Timeouts.Stall > 0 && c.Timeouts.Stall >= c.Timeouts.Run {
		add("timeouts.stall", "must be less than timeouts.run (%s), or it can never fire", c.Timeouts.Run)
	}

	if c.Retry.SelfCorrectionBudget < 0 {
		add("retry.self_correction_budget", "must not be negative, got %d", c.Retry.SelfCorrectionBudget)
	}
	for key, d := range map[string]Duration{
		"retry.cooldown_quota":       c.Retry.CooldownQuota,
		"retry.cooldown_rate_limit":  c.Retry.CooldownRateLimit,
		"retry.cooldown_unavailable": c.Retry.CooldownUnavailable,
	} {
		if d <= 0 {
			add(key, "must be positive, got %s", d)
		}
	}

	if c.Context.TokenBudget < 1 {
		add("context.token_budget", "must be at least 1, got %d", c.Context.TokenBudget)
	}

	if !c.Notifications.Mode.Valid() {
		add("notifications.mode", "unknown mode %q (valid: %s)", c.Notifications.Mode, joinModes())
	}
	if c.Notifications.RateLimitWindow < 0 {
		add("notifications.rate_limit_window", "must not be negative, got %s", c.Notifications.RateLimitWindow)
	}

	if len(errs) == 0 {
		return nil
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Key < errs[j].Key })
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedRoutes[V any](m map[core.Route]V) []core.Route {
	out := make([]core.Route, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func joinModes() string {
	parts := make([]string, len(AllNotifyModes))
	for i, m := range AllNotifyModes {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}
