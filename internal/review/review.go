// Package review runs an advisory review pass over a diff.
//
// It exists because serial mode makes human review latency the gate on a repository's
// throughput: anything that lets a human approve routine work confidently in seconds unblocks
// the queue. It is advisory and stays advisory — it annotates a diff for a person and never
// decides a ticket's fate, which is why nothing here can change state.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// Outcome is the overall verdict.
type Outcome string

// The outcomes. Deliberately three: a binary verdict invites treating it as a gate.
const (
	Pass     Outcome = "pass"
	Concerns Outcome = "concerns"
	Fail     Outcome = "fail"
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool { return o == Pass || o == Concerns || o == Fail }

// Severity ranks a finding.
type Severity string

// The severities.
const (
	High   Severity = "high"
	Medium Severity = "medium"
	Low    Severity = "low"
)

// Finding is one thing the reviewer noticed.
type Finding struct {
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	// Line is zero when the reviewer could not place it.
	Line      int    `json:"line"`
	Rationale string `json:"rationale"`
}

// Verdict is the reviewer's advisory opinion, structured so the Review screen can render it
// rather than dumping prose at a human who is trying to skim.
type Verdict struct {
	Overall  Outcome   `json:"overall"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	// Unavailable explains why there is no verdict. A review that failed must say so plainly
	// rather than looking like a clean pass, which is the one way an advisory tool can do
	// real harm.
	Unavailable string `json:"unavailable,omitempty"`
}

// Available reports whether the verdict carries an opinion.
func (v Verdict) Available() bool { return v.Unavailable == "" && v.Overall.Valid() }

// Model runs one prompt and returns its text.
//
// An interface so the reviewer is testable without a provider, and so a local model later is a
// different implementation rather than a rewrite.
//
// The project travels with the prompt because which model reviews is a project setting: a repo
// can pin its own review bucket, and an implementation that picked its model once could not
// honour that.
type Model interface {
	Complete(ctx context.Context, project core.Project, prompt string) (string, error)
}

// Request is everything the reviewer looks at.
type Request struct {
	Ticket     core.Ticket
	Project    core.Project
	Diff       git.Diff
	Validation validate.Results
	// Decisions are the change instructions a human has agreed since the ticket was written.
	// They amend the ticket. A reviewer shown only the ticket's original text flags every
	// deliberate narrowing as missing scope, and does it again on every round.
	Decisions []core.ChangeInstruction
	// Previous is the verdict on the round before this one, nil on a first review. Without it
	// each round starts from nothing and cannot say what was fixed and what is still open.
	Previous *Verdict
}

// Reviewer produces advisory verdicts.
type Reviewer struct {
	model Model
	// budget is the prompt's ceiling in characters. Diffs are truncated by whole files to fit.
	budget int
	// retryDelay is the wait after the first failed attempt; each later wait is one step longer.
	retryDelay time.Duration
	// sleep waits between attempts. A field so tests can observe it.
	sleep func(context.Context, time.Duration) error
}

// Attempts is how many times a review is tried before it is reported unavailable.
//
// Review CLIs fail transiently: an automatic review that died in under a second, with nothing
// on stdout, routinely succeeded when the human asked again moments later. Making the human
// the retry loop is the failure this exists to remove.
const Attempts = 3

// DefaultRetryDelay is the first wait between attempts. Short, because the failures it rides
// out cleared within seconds, and the human is waiting on the verdict.
const DefaultRetryDelay = 3 * time.Second

// DefaultBudget is a conservative character ceiling for the prompt.
//
// Characters rather than tokens: the reviewer has no tokeniser and does not need one, since the
// point is to stay well inside a limit rather than to sit exactly on it.
const DefaultBudget = 120_000

// New returns a reviewer over a model.
func New(m Model, budget int) *Reviewer {
	if budget <= 0 {
		budget = DefaultBudget
	}
	return &Reviewer{model: m, budget: budget, retryDelay: DefaultRetryDelay, sleep: sleepCtx}
}

// WithRetryDelay sets the first wait between attempts. Zero retries immediately.
func (r *Reviewer) WithRetryDelay(d time.Duration) *Reviewer {
	r.retryDelay = d
	return r
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Review produces a verdict, or an explanation of why there is none.
//
// It never returns an error for an unusable answer. A review that cannot run must not stop work
// reaching a human, so every failure becomes an unavailable verdict instead.
func (r *Reviewer) Review(ctx context.Context, req Request) Verdict {
	if r == nil || r.model == nil {
		return Verdict{Unavailable: "no review model is configured"}
	}
	if len(req.Diff.Files) == 0 {
		return Verdict{Unavailable: "there is no diff to review"}
	}

	prompt := BuildPrompt(req, r.budget)
	var last Verdict
	tried := 0
	for tried < Attempts {
		if tried > 0 {
			if err := r.sleep(ctx, time.Duration(tried)*r.retryDelay); err != nil {
				break
			}
		}
		tried++
		v, ok := r.once(ctx, req.Project, prompt)
		if ok {
			return v
		}
		last = v
		if ctx.Err() != nil {
			break
		}
	}
	// The count is part of the explanation, so an unavailable verdict is not mistaken for one
	// unlucky try the human should simply repeat.
	if tried > 1 {
		last.Unavailable = fmt.Sprintf("%s (after %d attempts)", last.Unavailable, tried)
	}
	return last
}

// once runs a single attempt. ok is false when there is no usable verdict.
func (r *Reviewer) once(ctx context.Context, project core.Project, prompt string) (Verdict, bool) {
	out, err := r.model.Complete(ctx, project, prompt)
	if err != nil {
		return Verdict{Unavailable: "the review model did not answer: " + err.Error()}, false
	}
	v, err := Parse(out)
	if err != nil {
		return Verdict{Unavailable: "the review model's answer could not be read: " + err.Error()}, false
	}
	return v, true
}

// Parse reads a verdict out of a model's answer.
//
// Models wrap JSON in prose and fences however they like, so this finds the object rather than
// insisting the whole answer be one. Anything it cannot read becomes an error, which the caller
// turns into an unavailable verdict.
func Parse(s string) (Verdict, error) {
	raw := extractJSON(s)
	if raw == "" {
		return Verdict{}, fmt.Errorf("no JSON object in the answer")
	}

	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("malformed JSON: %w", err)
	}
	v.Overall = Outcome(strings.ToLower(strings.TrimSpace(string(v.Overall))))
	if !v.Overall.Valid() {
		return Verdict{}, fmt.Errorf("overall %q is not pass, concerns or fail", v.Overall)
	}

	// Drop findings that carry nothing actionable rather than rendering empty rows.
	kept := v.Findings[:0]
	for _, f := range v.Findings {
		f.Severity = Severity(strings.ToLower(strings.TrimSpace(string(f.Severity))))
		switch f.Severity {
		case High, Medium, Low:
		default:
			f.Severity = Medium
		}
		if strings.TrimSpace(f.Rationale) == "" {
			continue
		}
		kept = append(kept, f)
	}
	v.Findings = kept
	return v, nil
}

// extractJSON returns the outermost JSON object in s, tolerating fences and surrounding prose.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside strings are text, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
