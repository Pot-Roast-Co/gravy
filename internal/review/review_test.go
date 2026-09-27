package review

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// stubModel answers with whatever it was given, or fails.
type stubModel struct {
	answer  string
	err     error
	prompts []string
}

func (m *stubModel) Complete(_ context.Context, _ core.Project, prompt string) (string, error) {
	m.prompts = append(m.prompts, prompt)
	if m.err != nil {
		return "", m.err
	}
	return m.answer, nil
}

func request() Request {
	return Request{
		Ticket:  core.Ticket{ID: "GR-1", Title: "Add Divide", Body: "handle the zero case"},
		Project: core.Project{Slug: "gravy"},
		Diff: git.Diff{Files: []git.FileDiff{
			{Path: "calc.go", Status: "modified", Additions: 6, Deletions: 0,
				Patch: "@@ -1,3 +1,9 @@\n+func Divide(a, b int) int { return a / b }"},
		}},
		Validation: validate.Results{{Step: "test", Outcome: validate.Passed}},
	}
}

// TestVerdictIsStructured is AC1: the Review screen renders fields, not prose.
func TestVerdictIsStructured(t *testing.T) {
	m := &stubModel{answer: `Here is my review:
` + "```json" + `
{"overall":"concerns","summary":"Divide by zero is unhandled.",
 "findings":[{"severity":"high","file":"calc.go","line":2,"rationale":"b may be zero"}]}
` + "```"}

	v := New(m, 0).Review(context.Background(), request())

	if !v.Available() {
		t.Fatalf("verdict is unavailable: %+v", v)
	}
	if v.Overall != Concerns {
		t.Errorf("overall = %q, want concerns", v.Overall)
	}
	if len(v.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", v.Findings)
	}
	// AC2: file and line where determinable.
	f := v.Findings[0]
	if f.File != "calc.go" || f.Line != 2 || f.Severity != High {
		t.Errorf("finding = %+v", f)
	}
}

// TestUnparseableDegradesToUnavailable is AC3. Looking like a clean pass is the one way an
// advisory tool does real harm.
func TestUnparseableDegradesToUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *stubModel
	}{
		{"prose only", &stubModel{answer: "Looks fine to me, ship it."}},
		{"malformed json", &stubModel{answer: `{"overall": "concerns", "findings": [`}},
		{"unknown outcome", &stubModel{answer: `{"overall":"probably fine"}`}},
		{"model failed", &stubModel{err: fmt.Errorf("quota exhausted")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.model, 0)
			r.sleep = noWait
			v := r.Review(context.Background(), request())
			if v.Available() {
				t.Fatalf("an unusable answer produced a usable verdict: %+v", v)
			}
			if v.Unavailable == "" {
				t.Error("no explanation for the missing verdict")
			}
			if v.Overall == Pass {
				t.Error("a failed review reported a pass")
			}
		})
	}
}

// TestNoModelIsNotAnError covers the pass being disabled entirely.
func TestNoModelIsNotAnError(t *testing.T) {
	v := New(nil, 0).Review(context.Background(), request())
	if v.Available() || v.Unavailable == "" {
		t.Errorf("verdict = %+v, want an explained unavailable", v)
	}
	var nilReviewer *Reviewer
	if got := nilReviewer.Review(context.Background(), request()); got.Available() {
		t.Error("a nil reviewer produced a verdict")
	}
}

// TestPromptSaysItIsAdvisory is the instruction the whole design rests on: a model told it is a
// gate writes like a gate.
func TestPromptSaysItIsAdvisory(t *testing.T) {
	p := BuildPrompt(request(), 0)
	if !strings.Contains(p, "ADVISORY") {
		t.Error("the prompt does not say the review is advisory")
	}
	if !strings.Contains(p, "does not approve, block or merge") {
		t.Error("the prompt does not say what the review cannot do")
	}
	for _, want := range []string{"GR-1", "Add Divide", "handle the zero case", "calc.go", "test"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt omits %q", want)
		}
	}
}

// TestLargeDiffTruncatesByFile is AC5. Half a hunk is worse than no hunk, because it reads as
// complete.
func TestLargeDiffTruncatesByFile(t *testing.T) {
	req := request()
	req.Diff.Files = nil
	for i := 0; i < 40; i++ {
		req.Diff.Files = append(req.Diff.Files, git.FileDiff{
			Path:      fmt.Sprintf("file%02d.go", i),
			Status:    "modified",
			Additions: 100,
			Patch:     "@@ -1 +1 @@\n" + strings.Repeat("+a line of diff\n", 400),
		})
	}

	budget := 20000
	p := BuildPrompt(req, budget)

	if len(p) > budget*2 {
		t.Errorf("prompt is %d chars against a %d budget", len(p), budget)
	}
	if !strings.Contains(p, "Omitted for length") {
		t.Fatalf("a truncated diff does not say what was left out")
	}
	// Whatever survived must be whole: a rendered file's patch appears in full.
	for _, f := range req.Diff.Files {
		header := fmt.Sprintf("### %s (modified", f.Path)
		if !strings.Contains(p, header) {
			continue
		}
		if !strings.Contains(p, f.Patch) {
			t.Errorf("%s was included but its patch was cut mid-way", f.Path)
		}
	}
}

// TestEmptyDiffNeedsNoReview avoids spending a model call on nothing.
func TestEmptyDiffNeedsNoReview(t *testing.T) {
	m := &stubModel{answer: `{"overall":"pass"}`}
	req := request()
	req.Diff.Files = nil

	v := New(m, 0).Review(context.Background(), req)
	if v.Available() {
		t.Errorf("verdict = %+v, want unavailable", v)
	}
	if len(m.prompts) != 0 {
		t.Error("the model was called for an empty diff")
	}
}

// TestFindingsWithoutRationaleAreDropped keeps empty rows off the review card.
func TestFindingsWithoutRationaleAreDropped(t *testing.T) {
	m := &stubModel{answer: `{"overall":"pass","findings":[
		{"severity":"low","file":"a.go","rationale":"   "},
		{"severity":"nonsense","file":"b.go","rationale":"this one is real"}]}`}

	v := New(m, 0).Review(context.Background(), request())
	if len(v.Findings) != 1 {
		t.Fatalf("findings = %+v, want the one with a rationale", v.Findings)
	}
	// An unrecognised severity becomes medium rather than being dropped or rendered raw.
	if v.Findings[0].Severity != Medium {
		t.Errorf("severity = %q, want medium", v.Findings[0].Severity)
	}
}

// TestParseFindsJSONAmongProse covers what models actually return.
func TestParseFindsJSONAmongProse(t *testing.T) {
	answers := []string{
		`{"overall":"pass"}`,
		"```json\n{\"overall\":\"pass\"}\n```",
		"Sure!\n\n{\"overall\":\"pass\",\"summary\":\"a } brace in a string\"}\n\nHope that helps.",
	}
	for i, a := range answers {
		v, err := Parse(a)
		if err != nil {
			t.Errorf("answer %d: %v", i, err)
			continue
		}
		if v.Overall != Pass {
			t.Errorf("answer %d: overall = %q", i, v.Overall)
		}
	}
}

// flakyModel fails a set number of times, then answers.
type flakyModel struct {
	failures int
	answer   string
	calls    int
}

func (m *flakyModel) Complete(context.Context, core.Project, string) (string, error) {
	m.calls++
	if m.calls <= m.failures {
		return "", fmt.Errorf("the reviewer failed: task_failure (no rule matched; defaulted)")
	}
	return m.answer, nil
}

func noWait(context.Context, time.Duration) error { return nil }

// TestTransientFailureIsRetried is the bug a human kept working around: the automatic review
// died, and pressing v a moment later worked. The retry belongs here, not in the human.
func TestTransientFailureIsRetried(t *testing.T) {
	m := &flakyModel{failures: Attempts - 1, answer: `{"overall":"pass","summary":"fine","findings":[]}`}
	r := New(m, 0)
	r.sleep = noWait

	v := r.Review(context.Background(), request())
	if !v.Available() || v.Overall != Pass {
		t.Fatalf("verdict = %+v, want the answer from the last attempt", v)
	}
	if m.calls != Attempts {
		t.Errorf("calls = %d, want %d", m.calls, Attempts)
	}
}

// TestPersistentFailureSaysHowHardItTried: after every attempt fails, the verdict says so, so
// the human knows pressing v once more is not the obvious fix.
func TestPersistentFailureSaysHowHardItTried(t *testing.T) {
	m := &flakyModel{failures: 100}
	r := New(m, 0)
	r.sleep = noWait

	v := r.Review(context.Background(), request())
	if v.Available() {
		t.Fatalf("a failing model produced a verdict: %+v", v)
	}
	if m.calls != Attempts {
		t.Errorf("calls = %d, want %d", m.calls, Attempts)
	}
	if !strings.Contains(v.Unavailable, fmt.Sprintf("after %d attempts", Attempts)) {
		t.Errorf("unavailable = %q, want the attempt count", v.Unavailable)
	}
}

// TestCancelledReviewStopsRetrying: a daemon shutting down must not sit out the backoff.
func TestCancelledReviewStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &flakyModel{failures: 100}
	r := New(m, 0)
	r.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }

	if v := r.Review(ctx, request()); v.Available() {
		t.Fatalf("verdict = %+v", v)
	}
	if m.calls != 1 {
		t.Errorf("calls = %d, want 1: a cancelled review kept trying", m.calls)
	}
}

// TestPromptCarriesDecisions is the other half of "the same review every round". The ticket
// said "ESPN and sheet"; the human narrowed it to ESPN; a reviewer shown only the ticket
// reported the narrowing as missing scope on every round.
func TestPromptCarriesDecisions(t *testing.T) {
	req := request()
	req.Decisions = []core.ChangeInstruction{
		{Correction: "  Narrow the ticket: projections come from ESPN only.  ",
			Preserve: []string{"consensus rank still uses the sheet", " "}},
		{Correction: " "},
	}
	p := BuildPrompt(req, 0)

	for _, want := range []string{
		"Decisions the human has made",
		"1. Narrow the ticket: projections come from ESPN only.",
		"Preserve: consensus rank still uses the sheet",
		"not the ticket's original wording",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if strings.Contains(p, "2. ") {
		t.Error("an empty instruction was rendered as a decision")
	}
	if strings.Index(p, "Decisions the human") > strings.Index(p, "## Diff") {
		t.Error("decisions should come before the diff, next to the ticket they amend")
	}
}

// TestPromptCarriesPreviousFindings: a round that cannot see the last one repeats it word for
// word, including checks nobody can run from a diff.
func TestPromptCarriesPreviousFindings(t *testing.T) {
	req := request()
	req.Previous = &Verdict{Overall: Concerns, Findings: []Finding{
		{Severity: Medium, File: "calc.go", Line: 2, Rationale: "b may be zero"},
		{Severity: Low, Rationale: "exports were not run"},
	}}
	p := BuildPrompt(req, 0)

	for _, want := range []string{
		"findings on the previous round",
		"[medium] calc.go:2: b may be zero",
		"[low] general: exports were not run",
		"not a defect in the diff",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// A first round, or a previous round with nothing to say, adds nothing.
	for _, prev := range []*Verdict{nil, {Unavailable: "no answer"}, {Overall: Pass}} {
		req.Previous = prev
		if strings.Contains(BuildPrompt(req, 0), "previous round") {
			t.Errorf("previous %+v rendered a section", prev)
		}
	}
}

// TestPromptAsksForEachCriterion is the ticket that was reviewed clean without half of itself.
// Its list asked for a settings page with a refresh button; no page was built, and nothing in
// the diff was wrong, so a reviewer asked only for defects had nothing to say.
func TestPromptAsksForEachCriterion(t *testing.T) {
	req := request()
	req.Ticket.Body = `First implementation of the provider.

Done looks like:
- Import the player pool. Upsert; re-runnable.
  - nested: injury status too
* Settings page showing the import with a refresh button
-
Not a bullet - even with a dash in it.`
	p := BuildPrompt(req, 0)

	for _, want := range []string{
		"Check each of the ticket's acceptance criteria",
		"- Import the player pool. Upsert; re-runnable.\n",
		"- nested: injury status too\n",
		"- Settings page showing the import with a refresh button\n",
		"judged by the decision",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	start := strings.Index(p, "Check each of the ticket's")
	section := p[start:]
	section = section[:strings.Index(section, "\n## ")]
	if strings.Contains(section, "Not a bullet") {
		t.Error("prose was taken for a criterion")
	}
	if got := strings.Count(section, "\n- "); got != 3 {
		t.Errorf("criteria listed = %d, want 3:\n%s", got, section)
	}
}

// A ticket with no list is reviewed as it always was.
func TestPromptWithoutCriteriaAddsNothing(t *testing.T) {
	if p := BuildPrompt(request(), 0); strings.Contains(p, "acceptance criteria") {
		t.Errorf("a ticket with no bullets got a criteria section:\n%s", p)
	}
}
