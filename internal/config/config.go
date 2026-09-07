package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// Config is the global configuration in ~/.gravy/config.yaml.
//
// Per-project settings are not here: they live in SQLite so they can be edited from the TUI
// without hand-editing a file (ARCHITECTURE.md §5).
type Config struct {
	Concurrency   Concurrency             `yaml:"concurrency"`
	Providers     map[string]Provider     `yaml:"providers"`
	Routes        map[core.Route][]string `yaml:"routes"`
	Timeouts      Timeouts                `yaml:"timeouts"`
	Retry         Retry                   `yaml:"retry"`
	Context       Context                 `yaml:"context"`
	Notifications Notifications           `yaml:"notifications"`
	Retention     Retention               `yaml:"retention"`
}

// Retention controls how long artefacts are kept on disk.
type Retention struct {
	// RunLogs is how long a finished run's agent output and event stream are kept.
	//
	// Summaries are deliberately not covered by this: they live in the database and are what a
	// dependent ticket reads as fact, so they must outlive the logs they were derived from.
	// Zero keeps logs forever.
	RunLogs Duration `yaml:"run_logs"`
}

// Concurrency controls how many runs may execute at once.
type Concurrency struct {
	// Workers is the global pool shared across projects. Project availability, not this
	// number, is what limits any single repository: N projects in serial mode run N agents
	// concurrently, one per repository.
	Workers int `yaml:"workers"`

	// Routes caps how many tickets on a given route may be in flight at once, across every
	// project. A route is a bucket of agent capacity — tickets ask for one by name — so
	// "two planning agents and four implementation agents" is expressed here rather than by
	// counting workers and hoping.
	//
	// An unlisted route is limited only by Workers.
	Routes map[core.Route]int `yaml:"routes,omitempty"`
}

// Provider is a coding agent CLI Gravy may drive.
//
// Gravy drives the CLIs the user has already authenticated. It does not want provider
// credentials and does not store them, so there is no key field here by design.
type Provider struct {
	Enabled bool `yaml:"enabled"`
	// Command is the executable name or path, looked up on PATH when not absolute.
	Command string `yaml:"command"`
}

// Timeouts bound a run so that a wedged agent cannot hold a worker forever.
type Timeouts struct {
	// Run is the wall-clock cap on a single agent run.
	//
	// It must comfortably exceed a provider's internal retry window or healthy-but-slow runs
	// are killed as false positives: an unauthenticated claude CLI was measured retrying
	// silently for 188 seconds before reporting a 401 (docs/SPIKE-claude-code.md, F2).
	Run Duration `yaml:"run"`
	// ValidationStep caps a single validation command.
	ValidationStep Duration `yaml:"validation_step"`
	// Stall is how long a run may emit no events before it is treated as hung.
	//
	// This is the better liveness signal, because a wedged run and a slow one are
	// indistinguishable by process state alone. Zero disables stall detection.
	Stall Duration `yaml:"stall"`
}

// Retry is the self-correction budget and the cooldowns applied to unavailable models.
type Retry struct {
	// SelfCorrectionBudget is how many times a red validation goes back to the agent before
	// the ticket parks in Needs You.
	SelfCorrectionBudget int `yaml:"self_correction_budget"`
	// CooldownQuota applies to a genuinely exhausted quota. A provider-reported reset time is
	// preferred over this value whenever one is available.
	CooldownQuota Duration `yaml:"cooldown_quota"`
	// CooldownRateLimit applies to a transient rate limit.
	CooldownRateLimit Duration `yaml:"cooldown_rate_limit"`
	// CooldownUnavailable applies to a broken CLI or a provider outage.
	CooldownUnavailable Duration `yaml:"cooldown_unavailable"`
}

// Context bounds what an agent is given.
type Context struct {
	// TokenBudget is the hard cap on assembled per-ticket context.
	TokenBudget int `yaml:"token_budget"`
}

// NotifyMode is how loudly Gravy asks for attention.
type NotifyMode string

// The notification modes.
const (
	NotifyOff       NotifyMode = "off"
	NotifyBell      NotifyMode = "bell"
	NotifyBellAndOS NotifyMode = "bell_and_os"
)

// AllNotifyModes lists every mode.
var AllNotifyModes = []NotifyMode{NotifyOff, NotifyBell, NotifyBellAndOS}

// Valid reports whether m is a known mode.
func (m NotifyMode) Valid() bool {
	for _, k := range AllNotifyModes {
		if k == m {
			return true
		}
	}
	return false
}

// Notifications configures how the human finds out Gravy needs them.
type Notifications struct {
	Mode NotifyMode `yaml:"mode"`
	// RateLimitWindow coalesces a burst of attention items into a single alert, so a wave of
	// finishing runs does not produce a wave of pings.
	RateLimitWindow Duration `yaml:"rate_limit_window"`
}

// Default returns the configuration Gravy writes on first run.
//
// defaultFile must stay in agreement with this; TestDefaultFileMatchesDefault enforces it.
func Default() Config {
	return Config{
		Concurrency: Concurrency{Workers: 4},
		Providers: map[string]Provider{
			"claude-code": {Enabled: true, Command: "claude"},
			"codex":       {Enabled: true, Command: "codex"},
		},
		Routes: map[core.Route][]string{
			// codex is deliberately absent: on a ChatGPT-account login it rejects every
			// explicit model name, so a route naming one sends work to a guaranteed
			// failure. The default file documents `codex/default` as the way to enable it.
			core.RouteImplementation: {"claude-code/sonnet", "claude-code/haiku"},
			core.RouteReview:         {"claude-code/sonnet"},
			core.RouteCheap:          {"claude-code/haiku"},
			core.RouteStandard:       {"claude-code/sonnet"},
			core.RouteStrong:         {"claude-code/opus"},
			core.RoutePlanning:       {"claude-code/opus"},
			// RouteLocal resolves to nothing in v0.1 and falls through to the next choice.
			core.RouteLocal: {},
		},
		Timeouts: Timeouts{
			Run:            Duration(30 * time.Minute),
			ValidationStep: Duration(10 * time.Minute),
			Stall:          Duration(5 * time.Minute),
		},
		Retry: Retry{
			SelfCorrectionBudget: 1,
			CooldownQuota:        Duration(time.Hour),
			CooldownRateLimit:    Duration(5 * time.Minute),
			CooldownUnavailable:  Duration(15 * time.Minute),
		},
		Context:       Context{TokenBudget: 60000},
		Notifications: Notifications{Mode: NotifyBellAndOS, RateLimitWindow: Duration(30 * time.Second)},
		Retention:     Retention{RunLogs: Duration(14 * 24 * time.Hour)},
	}
}

// ParseChoice splits a "provider/model" route entry.
func ParseChoice(s string) (core.Choice, error) {
	provider, model, ok := strings.Cut(s, "/")
	if !ok || provider == "" || model == "" {
		return core.Choice{}, fmt.Errorf("%q is not \"provider/model\"", s)
	}
	return core.Choice{ProviderID: provider, Model: model}, nil
}

// FormatChoice renders a choice as it is written in the file.
func FormatChoice(c core.Choice) string { return c.ProviderID + "/" + c.Model }

// RouteChoices returns the ordered choices for a route. A route with no configured choices
// returns nil; callers treat that as "fall through".
func (c Config) RouteChoices(r core.Route) ([]core.Choice, error) {
	entries, ok := c.Routes[r]
	if !ok {
		return nil, nil
	}
	out := make([]core.Choice, 0, len(entries))
	for i, e := range entries {
		ch, err := ParseChoice(e)
		if err != nil {
			return nil, fmt.Errorf("routes.%s[%d]: %w", r, i, err)
		}
		out = append(out, ch)
	}
	return out, nil
}

// EnabledProviders returns the ids of enabled providers, sorted for stable output.
func (c Config) EnabledProviders() []string {
	out := make([]string, 0, len(c.Providers))
	for id, p := range c.Providers {
		if p.Enabled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
