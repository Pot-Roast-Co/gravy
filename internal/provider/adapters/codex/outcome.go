package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// usageTotals is what turn.completed reports.
type usageTotals struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	// Turns counts completed turns, since codex reports usage per turn rather than a total.
	Turns int
}

// usageFrom accumulates a turn.completed line, or returns nil for anything else.
func usageFrom(line string) *usageTotals {
	var se streamEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &se); err != nil {
		return nil
	}
	if se.Type != typeTurnCompleted || se.Usage == nil {
		return nil
	}
	return &usageTotals{
		InputTokens:       se.Usage.InputTokens,
		CachedInputTokens: se.Usage.CachedInputTokens,
		OutputTokens:      se.Usage.OutputTokens,
		Turns:             1,
	}
}

// computeOutcome turns a finished process into an Outcome.
//
// Separated from run so the judgement can be tested without a live process. This is where a CLI
// that misreports its own success gets corrected, and every correction is invisible to an
// integration test that does not reproduce the exact failure.
func computeOutcome(p *Provider, status host.ExitStatus, usage *usageTotals, stdout, stderr, threadID string) provider.Outcome {
	out := provider.Outcome{
		ExitCode: status.Code,
		TimedOut: status.TimedOut,
		Session:  provider.SessionRef{ProviderID: ID, ID: threadID},
	}

	if status.TimedOut {
		out.Class = provider.Timeout
		out.Note = fmt.Sprintf("killed after %s without completing", status.Duration.Round(time.Second))
		return out
	}

	if usage != nil {
		// Cached input still cost a request and is counted, matching how the claude-code
		// adapter totals cache reads.
		out.TokensIn = usage.InputTokens + usage.CachedInputTokens
		out.TokensOut = usage.OutputTokens
		out.Turns = usage.Turns
	}
	// CostUSD stays nil: codex reports token counts but not a price, and inventing one from a
	// hard-coded rate card would be a number that silently goes stale.

	// A turn.failed or error event means the run failed even when the process exits 0, which
	// was observed: a rejected model produced both an error event and exit 1, but the two are
	// not guaranteed to travel together.
	if failure := lastError(stdout); failure != "" {
		c := p.Classify(status.Code, stdout, stderr)
		out.Class = c.Class.Effective()
		out.Note = fmt.Sprintf("%s: %s", c.Note(), truncate(failure, 200))
		return out
	}

	if status.Code != 0 {
		c := p.Classify(status.Code, stdout, stderr)
		out.Class = c.Class.Effective()
		out.Note = c.Note()
		return out
	}

	out.Class = provider.Success
	return out
}

// lastError returns the human-readable text of the final error or turn.failed event, if any.
func lastError(stdout string) string {
	var found string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var se streamEvent
		if err := json.Unmarshal([]byte(line), &se); err != nil {
			continue
		}
		if se.Type == typeError || se.Type == typeTurnFailed {
			if txt := se.errorText(); txt != "" {
				found = txt
			}
		}
	}
	return found
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
