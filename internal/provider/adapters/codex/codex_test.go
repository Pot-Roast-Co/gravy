package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "fixtures", "codex", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// TestParseRealSuccessStream reads the captured run rather than a hand-written imitation, which
// is the only way a parser stays honest about what the CLI actually emits.
func TestParseRealSuccessStream(t *testing.T) {
	var (
		kinds    = map[provider.EventKind]int{}
		threadID string
		usage    *usageTotals
	)
	for _, line := range strings.Split(fixture(t, "exec-success.jsonl"), "\n") {
		if id := threadIDFrom(line); id != "" {
			threadID = id
		}
		if u := usageFrom(line); u != nil {
			usage = u
		}
		for _, e := range parseLine(line) {
			kinds[e.Kind]++
			if e.Raw == "" {
				t.Error("an event lost its raw line, which is what makes a parsing gap diagnosable")
			}
		}
	}

	if threadID == "" {
		t.Error("no thread id was scraped; resume would be impossible")
	}
	// codex assigns its own session id, so it must be read from the stream.
	if len(threadID) < 8 {
		t.Errorf("thread id %q looks wrong", threadID)
	}
	for _, want := range []provider.EventKind{
		provider.EventStarted, provider.EventMessage,
		provider.EventToolUse, provider.EventToolResult, provider.EventUsage,
	} {
		if kinds[want] == 0 {
			t.Errorf("no %s events parsed from a real run: %v", want, kinds)
		}
	}
	if usage == nil || usage.OutputTokens == 0 {
		t.Errorf("token usage was not read from turn.completed: %+v", usage)
	}
}

// TestParseRejectedModelStream covers the failure this adapter has real evidence for.
func TestParseRejectedModelStream(t *testing.T) {
	raw := fixture(t, "exec-model-rejected.jsonl")

	var sawError bool
	for _, line := range strings.Split(raw, "\n") {
		for _, e := range parseLine(line) {
			if e.Kind == provider.EventError {
				sawError = true
				// The nested envelope must be unwrapped to something a human can read.
				if strings.Contains(e.Text, `\"`) {
					t.Errorf("error text is still escaped JSON: %q", e.Text)
				}
				if !strings.Contains(e.Text, "not supported") {
					t.Errorf("error text lost the reason: %q", e.Text)
				}
			}
		}
	}
	if !sawError {
		t.Fatal("no error event parsed from a failed run")
	}
	if got := lastError(raw); !strings.Contains(got, "not supported") {
		t.Errorf("lastError = %q", got)
	}
}

// TestClassify is GR-013 AC3. Every case below is either observed or explicitly marked
// unverified in the matcher table.
func TestClassify(t *testing.T) {
	p := New()
	tests := []struct {
		name   string
		exit   int
		stdout string
		want   provider.FailureClass
	}{
		{"observed: rejected model", 1, fixture(t, "exec-model-rejected.jsonl"), provider.TaskFailure},
		{"unverified: 401", 1, `{"type":"error","message":"{\"status\":401}"}`, provider.AuthExpired},
		{"unverified: quota", 1, `{"type":"error","message":"usage limit reached"}`, provider.QuotaExhausted},
		{"unverified: 429", 1, `{"type":"error","message":"{\"status\":429}"}`, provider.RateLimited},
		{"unverified: 503", 1, `{"type":"error","message":"{\"status\":503}"}`, provider.ProviderUnavailable},
		{"cli missing", 127, "", provider.ProviderUnavailable},
		{"killed", 137, "", provider.Timeout},
		// The invariant that matters most: anything unrecognised is the agent's failure, never
		// a quota condition. A false quota escalates work up the fallback chain toward the
		// most expensive model, which is the outcome routing exists to prevent.
		{"unknown", 1, "something nobody has seen before", provider.TaskFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Classify(tc.exit, tc.stdout, "")
			if got.Class.Effective() != tc.want {
				t.Errorf("class = %v (rule %q), want %v", got.Class, got.Rule, tc.want)
			}
		})
	}
}

// TestUnknownIsNeverAQuotaCondition states the invariant on its own, because it is the one that
// costs money when it breaks.
func TestUnknownIsNeverAQuotaCondition(t *testing.T) {
	c := New().Classify(1, "mystery failure", "also a mystery")
	if c.Class.Effective().IsQuotaCondition() {
		t.Fatalf("an unrecognised failure was classified as %v", c.Class)
	}
	if !c.Defaulted {
		t.Error("an unmatched classification should report that it defaulted")
	}
}

// TestOutcomeFromRealStreams checks the judgement without a live process.
func TestOutcomeFromRealStreams(t *testing.T) {
	p := New()

	t.Run("success", func(t *testing.T) {
		raw := fixture(t, "exec-success.jsonl")
		var usage *usageTotals
		for _, l := range strings.Split(raw, "\n") {
			if u := usageFrom(l); u != nil {
				usage = u
			}
		}
		out := computeOutcome(p, host.ExitStatus{Code: 0}, usage, raw, "", "thread-1")
		if out.Class != provider.Success {
			t.Errorf("class = %v, want success", out.Class)
		}
		if out.TokensIn == 0 || out.TokensOut == 0 {
			t.Errorf("tokens not carried: in=%d out=%d", out.TokensIn, out.TokensOut)
		}
		if out.CostUSD != nil {
			t.Error("codex reports no cost; inventing one would go stale silently")
		}
		if out.Session.ID != "thread-1" || out.Session.ProviderID != ID {
			t.Errorf("session = %+v", out.Session)
		}
	})

	t.Run("rejected model", func(t *testing.T) {
		raw := fixture(t, "exec-model-rejected.jsonl")
		out := computeOutcome(p, host.ExitStatus{Code: 1}, nil, raw, "", "thread-2")
		if out.Class != provider.TaskFailure {
			t.Errorf("class = %v, want task_failure", out.Class)
		}
		if !strings.Contains(out.Note, "not supported") {
			t.Errorf("note lost the evidence: %q", out.Note)
		}
	})

	t.Run("an error event beats a zero exit", func(t *testing.T) {
		// A CLI that reports failure in its stream but exits 0 must not be read as success.
		raw := `{"type":"turn.failed","error":{"message":"{\"status\":500}"}}`
		out := computeOutcome(p, host.ExitStatus{Code: 0}, nil, raw, "", "t")
		if out.Class == provider.Success {
			t.Error("a failed turn was reported as a successful run")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		out := computeOutcome(p, host.ExitStatus{Code: 137, TimedOut: true, Duration: 90 * time.Second}, nil, "", "", "t")
		if out.Class != provider.Timeout {
			t.Errorf("class = %v, want timeout", out.Class)
		}
	})
}

// TestRunArgsAreHonestAboutWhatCodexCannotDo is GR-013 AC2: unsupported features are reported,
// not emulated.
func TestRunArgsAreHonestAboutWhatCodexCannotDo(t *testing.T) {
	args := New().runArgs(provider.AgentTask{
		WorktreePath: "/work/repo",
		Prompt:       "do the thing",
		Model:        "gpt-5-codex",
		MaxTurns:     7, // codex has no turn cap
	})

	joined := strings.Join(args, " ")
	for _, want := range []string{"exec", "--json", "--skip-git-repo-check", "-C /work/repo", "-s workspace-write", "-m gpt-5-codex"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args omit %q: %v", want, args)
		}
	}
	// MaxTurns must not be faked into some other flag.
	if strings.Contains(joined, "7") {
		t.Errorf("MaxTurns leaked into the invocation: %v", args)
	}
	// The prompt is deliberately absent: codex reads it from stdin when no positional prompt
	// is given, and an argument is capped at 128KiB while a planning prompt is not.
	if strings.Contains(joined, "do the thing") {
		t.Errorf("the prompt is an argument again, which caps it at 128KiB: %v", args)
	}
}

// TestResumeRefusesAForeignSession keeps one provider from resuming another's thread.
func TestResumeRefusesAForeignSession(t *testing.T) {
	_, err := New().Resume(t.Context(), nil, provider.SessionRef{ProviderID: "claude-code", ID: "x"}, provider.AgentTask{Prompt: "go on"})
	if err == nil || !strings.Contains(err.Error(), "belongs to provider") {
		t.Errorf("err = %v, want a refusal naming the other provider", err)
	}
}

// TestSatisfiesTheInterface is the point of the whole ticket: a second provider proves the
// abstraction holds.
func TestSatisfiesTheInterface(t *testing.T) {
	var _ provider.Provider = (*Provider)(nil)
}

// TestDefaultModelIsNotSentToTheCLI covers the difference that actually broke a real run: a
// ChatGPT-account login rejects every explicit model name, so the sentinel must produce no
// --model flag at all.
func TestDefaultModelIsNotSentToTheCLI(t *testing.T) {
	base := provider.AgentTask{WorktreePath: "/work/repo", Prompt: "go"}

	for _, model := range []string{"", DefaultModel} {
		task := base
		task.Model = model
		if joined := strings.Join(New().runArgs(task), " "); strings.Contains(joined, "-m") {
			t.Errorf("model %q produced a --model flag: %s", model, joined)
		}
	}

	// An explicit choice is still the user's to make, and is passed through untouched.
	task := base
	task.Model = "o3"
	if joined := strings.Join(New().runArgs(task), " "); !strings.Contains(joined, "-m o3") {
		t.Errorf("an explicit model was dropped: %s", joined)
	}
}

// TestModelsReportsOnlyWhatIsVerifiable is GR-013 AC2 stated directly: capabilities the CLI does
// not have are reported honestly rather than emulated with a plausible-looking list.
// TestModelsAreSuggestionsNotAWhitelist records a correction.
//
// This test used to assert the opposite: that Models returns only the default sentinel, and that
// listing any "gpt-" name would "send work to a guaranteed failure". That was inferred from
// watching `gpt-5-codex` and `gpt-5` return 400 on a ChatGPT-account login — but those names had
// simply stopped existing. `gpt-5.6-sol` was then verified working on exactly such a login, so
// the rule was never "no model may be named".
//
// The list is therefore offered as suggestions, and ModelsAreOpen says so, because a
// hand-transcribed list goes stale the day its vendor ships a model.
func TestModelsAreSuggestionsNotAWhitelist(t *testing.T) {
	p := New()
	models, err := p.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) < 2 {
		t.Fatalf("models = %+v, want the default sentinel and some real names", models)
	}

	byID := map[string]bool{}
	for _, m := range models {
		if m.ID == "" || m.Name == "" {
			t.Errorf("model %+v is missing an id or a name", m)
		}
		byID[m.ID] = true
	}
	// The sentinel has to stay: it is the only thing that works on a login that cannot name
	// models, and it means "do not pass --model".
	if !byID[DefaultModel] {
		t.Error("the default sentinel is gone; a login that cannot name models has nothing to use")
	}
	// The name that disproved the old assumption.
	if !byID["gpt-5.6-sol"] {
		t.Error("gpt-5.6-sol is not listed, though it was verified working on a ChatGPT login")
	}

	if !p.ModelsAreOpen() {
		t.Error("the list is hand-transcribed, so it must not be treated as a whitelist")
	}
}
