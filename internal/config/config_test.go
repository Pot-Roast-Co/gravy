package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestDefaultFileMatchesDefault is the anti-drift test: the documented file Gravy writes on
// first run must parse to exactly the defaults the code uses. Without this, a default could be
// changed in one place and silently disagree with the other.
func TestDefaultFileMatchesDefault(t *testing.T) {
	got, err := Parse([]byte(defaultFile), "default")
	if err != nil {
		t.Fatalf("the default file does not parse: %v", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("default file parses to a different config than Default()\n got: %+v\nwant: %+v", got, want)
	}
}

// TestDefaultIsValid guards against shipping a default that fails its own validation.
func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() is not valid: %v", err)
	}
}

// TestDefaultFileDocumentsEverySetting checks AC4 mechanically: every YAML key in the default
// file carries a comment somewhere above it in its block.
func TestDefaultFileDocumentsEverySetting(t *testing.T) {
	for _, section := range []string{
		"concurrency", "providers", "routes", "timeouts", "retry", "context", "notifications",
	} {
		if !strings.Contains(defaultFile, "\n"+section+":") {
			t.Errorf("default file has no %q section", section)
		}
	}
	if strings.Count(defaultFile, "#") < 15 {
		t.Error("default file appears to be undocumented")
	}
}

func TestLoadCreatesDefaultOnFirstRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gravy") // deliberately does not exist yet

	cfg, created, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !created {
		t.Error("created = false on first run, want true")
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Error("first run did not return the default config")
	}

	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if !strings.Contains(string(b), "#") {
		t.Error("written config has no comments; defaults must be documented in the file")
	}

	// Loading again must not report a first run, and must produce the same config.
	cfg2, created2, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if created2 {
		t.Error("created = true on second load")
	}
	if !reflect.DeepEqual(cfg, cfg2) {
		t.Error("second load produced a different config")
	}
}

// TestRoundTrip is AC3: Load -> Save -> Load is stable.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	first, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Change something, as onboarding write-back would.
	first.Concurrency.Workers = 9
	first.Notifications.Mode = NotifyBell
	first.Routes[core.RouteCheap] = []string{"codex/gpt-5-codex"}

	if err := Save(dir, first); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second, created, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created {
		t.Error("reload reported a first run after Save")
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("round trip changed the config\n got: %+v\nwant: %+v", second, first)
	}

	// And again, to prove it is stable rather than merely reversible once.
	if err := Save(dir, second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	third, _, err := Load(dir)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if !reflect.DeepEqual(second, third) {
		t.Error("second round trip was not stable")
	}
}

func TestPartialConfigTakesDefaults(t *testing.T) {
	got, err := Parse([]byte("concurrency:\n  workers: 2\n"), "partial.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Concurrency.Workers != 2 {
		t.Errorf("workers = %d, want 2", got.Concurrency.Workers)
	}
	// Everything absent must fall back, so adding a setting never breaks an existing install.
	if got.Context.TokenBudget != Default().Context.TokenBudget {
		t.Errorf("token_budget = %d, want the default %d", got.Context.TokenBudget, Default().Context.TokenBudget)
	}
	if got.Notifications.Mode != Default().Notifications.Mode {
		t.Errorf("mode = %q, want the default", got.Notifications.Mode)
	}
}

func TestEmptyConfigIsDefault(t *testing.T) {
	got, err := Parse(nil, "empty.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Error("an empty file did not produce the defaults")
	}
}

// TestParseErrors is AC2: malformed YAML and unknown routes name the key and the line.
func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		// wantSubstrings must all appear in the error message.
		wantSubstrings []string
	}{
		{
			name:           "malformed yaml reports a line",
			yaml:           "concurrency:\n  workers: 4\n   bad indent here\n",
			wantSubstrings: []string{"bad.yaml", "line"},
		},
		{
			name:           "unknown top-level key",
			yaml:           "concurrenc:\n  workers: 4\n",
			wantSubstrings: []string{"concurrenc"},
		},
		{
			// Bucket names are the user's to choose, so a name Gravy does not ship with is
			// accepted — only one that would be ambiguous in a config file or on a command
			// line is refused. A typo in a bucket NAME is therefore indistinguishable from a
			// new bucket; what catches it is a ticket asking for a bucket that is not
			// configured, which api.knownRoute refuses.
			name:           "unusable bucket name",
			yaml:           "routes:\n  two words:\n    - claude-code/sonnet\n",
			wantSubstrings: []string{"routes.two words", "usable bucket name", "bad.yaml:2"},
		},
		{
			name:           "route entry that is not provider/model",
			yaml:           "routes:\n  cheap:\n    - sonnet\n",
			wantSubstrings: []string{"routes.cheap[0]", "provider/model", "bad.yaml:3"},
		},
		{
			name:           "route referencing an unconfigured provider",
			yaml:           "routes:\n  cheap:\n    - nope/sonnet\n",
			wantSubstrings: []string{"routes.cheap[0]", "nope", "not configured", "bad.yaml:3"},
		},
		{
			name:           "zero workers",
			yaml:           "concurrency:\n  workers: 0\n",
			wantSubstrings: []string{"concurrency.workers", "at least 1", "bad.yaml:2"},
		},
		{
			name:           "unknown notification mode",
			yaml:           "notifications:\n  mode: shout\n",
			wantSubstrings: []string{"notifications.mode", "shout", "bell_and_os", "bad.yaml:2"},
		},
		{
			name:           "unparseable duration",
			yaml:           "timeouts:\n  run: 30 minutes\n",
			wantSubstrings: []string{"duration"},
		},
		{
			name:           "negative token budget",
			yaml:           "context:\n  token_budget: 0\n",
			wantSubstrings: []string{"context.token_budget", "bad.yaml:2"},
		},
		{
			// A stall timeout at or above the run timeout can never fire, quietly disabling
			// the only detector that separates a wedged run from a slow one.
			name:           "stall timeout that can never fire",
			yaml:           "timeouts:\n  run: 10m\n  stall: 10m\n",
			wantSubstrings: []string{"timeouts.stall", "never fire", "bad.yaml:3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), "bad.yaml")
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			msg := err.Error()
			for _, want := range tt.wantSubstrings {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// TestValidationErrorsAreInspectable checks the typed error surface, not just the string.
func TestValidationErrorsAreInspectable(t *testing.T) {
	_, err := Parse([]byte("concurrency:\n  workers: 0\n"), "bad.yaml")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error is not ErrInvalid: %v", err)
	}
	var es Errors
	if !errors.As(err, &es) {
		t.Fatalf("error is not Errors: %v", err)
	}
	if len(es) != 1 {
		t.Fatalf("got %d errors, want 1", len(es))
	}
	if es[0].Key != "concurrency.workers" || es[0].Line != 2 {
		t.Errorf("got key %q line %d, want concurrency.workers line 2", es[0].Key, es[0].Line)
	}
}

// TestAllProblemsReportedAtOnce: fixing a config should be one pass, not a guessing game.
func TestAllProblemsReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte("concurrency:\n  workers: 0\ncontext:\n  token_budget: 0\nnotifications:\n  mode: shout\n"), "bad.yaml")
	if err == nil {
		t.Fatal("want an error")
	}
	var es Errors
	if !errors.As(err, &es) {
		t.Fatalf("error is not Errors: %v", err)
	}
	if len(es) != 3 {
		t.Fatalf("got %d errors, want 3: %v", len(es), err)
	}
}

func TestSaveRefusesInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	bad := Default()
	bad.Concurrency.Workers = 0
	if err := Save(dir, bad); err == nil {
		t.Fatal("Save accepted an invalid config")
	}
	if _, err := os.Stat(Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("Save wrote a file despite refusing the config")
	}
}

func TestHomeRespectsEnv(t *testing.T) {
	t.Setenv(EnvHome, "/tmp/somewhere")
	got, err := Home()
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	if got != "/tmp/somewhere" {
		t.Errorf("Home() = %q, want /tmp/somewhere", got)
	}
}

func TestRouteChoices(t *testing.T) {
	// A route written out explicitly rather than whichever one Default() ships: what is being
	// tested is that parsing preserves order, not what the defaults happen to contain.
	c := Default()
	c.Routes[core.RouteImplementation] = []string{"claude-code/sonnet", "codex/default", "claude-code/haiku"}

	got, err := c.RouteChoices(core.RouteImplementation)
	if err != nil {
		t.Fatalf("RouteChoices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d choices, want 3", len(got))
	}
	if got[0].ProviderID != "claude-code" || got[0].Model != "sonnet" {
		t.Errorf("first choice = %+v, want claude-code/sonnet", got[0])
	}
	// Ordering is the fallback order and must be preserved exactly.
	if FormatChoice(got[1]) != "codex/default" {
		t.Errorf("second choice = %q", FormatChoice(got[1]))
	}
	if FormatChoice(got[2]) != "claude-code/haiku" {
		t.Errorf("third choice = %q", FormatChoice(got[2]))
	}

	// An unconfigured route returns nothing rather than erroring: it falls through.
	none, err := c.RouteChoices(core.Route("nonexistent"))
	if err != nil || none != nil {
		t.Errorf("unconfigured route = %v, %v; want nil, nil", none, err)
	}
}

func TestParseChoice(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"claude-code/sonnet", false},
		{"codex/gpt-5-codex", false},
		{"claude-code/claude-opus-5", false},
		{"sonnet", true},
		{"/sonnet", true},
		{"claude-code/", true},
		{"", true},
	}
	for _, tt := range tests {
		got, err := ParseChoice(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseChoice(%q) = %+v, want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseChoice(%q): %v", tt.in, err)
			continue
		}
		if FormatChoice(got) != tt.in {
			t.Errorf("round trip of %q gave %q", tt.in, FormatChoice(got))
		}
	}
}

func TestEnabledProviders(t *testing.T) {
	c := Default()
	c.Providers["disabled-one"] = Provider{Enabled: false, Command: "nope"}
	got := c.EnabledProviders()
	want := []string{"claude-code", "codex", "copilot"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnabledProviders() = %v, want %v", got, want)
	}
}

func TestReadMissingKeepsDefaultsInMemory(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatal("read wrote config before approval")
	}
}
