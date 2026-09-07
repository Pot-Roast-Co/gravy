package codex

import (
	"regexp"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// matcher is the classification table for the codex CLI.
//
// codex nests the upstream API error as a JSON string inside its own event, so the status code
// travels in the stream verbatim:
//
//	{"type":"error","message":"{\"type\":\"error\",\"status\":400,\"error\":{...}}"}
//
// Matching the status is therefore far more stable than matching prose, which is reworded
// freely. Order matters: the first match wins, so specific rules precede general ones.
//
// OBSERVED against codex-cli 0.152.1: the 400 invalid-model case only. The 401, 429, 5xx and
// quota rules below are written against the documented shape of the same envelope and are
// marked as unverified rather than presented as evidence — an invented pattern is how a
// classifier drifts into misclassifying, and mislabelling a task failure as a quota condition
// sends the queue up the fallback chain toward the most expensive model.
var matcher = provider.NewMatcher(
	provider.Rule{
		// OBSERVED, for a nonsense name and for two real ones. On a ChatGPT-account login
		// `codex exec -m <anything>` gives exit 1 and:
		//   "The '<name>' model is not supported when using Codex with a ChatGPT account."
		// with "status":400 — including for gpt-5 and gpt-5-codex. Passing no --model at all
		// is what works there, which is why codex.DefaultModel exists.
		// A configuration error, not a provider outage: escalating it would spend the
		// expensive route on a request guaranteed to fail the same way.
		Name:    "model not supported",
		Class:   provider.TaskFailure,
		Pattern: regexp.MustCompile(`(?i)model is not supported|invalid_request_error`),
	},
	provider.Rule{
		// UNVERIFIED: could not be induced without logging out.
		Name:    "http 401 unauthenticated",
		Class:   provider.AuthExpired,
		Pattern: regexp.MustCompile(`(?i)\\?"status\\?":\s*401|not logged in|please run .?codex login|unauthorized`),
	},
	provider.Rule{
		// UNVERIFIED: an exhausted plan window could not be induced.
		Name:    "usage limit reached",
		Class:   provider.QuotaExhausted,
		Pattern: regexp.MustCompile(`(?i)usage limit|quota (has been )?exceeded|out of credits|insufficient_quota`),
	},
	provider.Rule{
		// UNVERIFIED.
		Name:    "http 429 rate limited",
		Class:   provider.RateLimited,
		Pattern: regexp.MustCompile(`(?i)\\?"status\\?":\s*429|rate.?limit`),
	},
	provider.Rule{
		// UNVERIFIED. 5xx is the provider's side failing, not the agent's work.
		Name:    "upstream unavailable",
		Class:   provider.ProviderUnavailable,
		Pattern: regexp.MustCompile(`(?i)\\?"status\\?":\s*5\d\d|server_error|service unavailable`),
	},
	provider.Rule{
		// 127 is the shell's "command not found": the CLI is not installed where we looked.
		Name:      "cli not found",
		Class:     provider.ProviderUnavailable,
		ExitCodes: []int{127},
	},
	provider.Rule{
		// 137 is 128+SIGKILL, which is how host.LocalHost ends a timed-out or cancelled run.
		Name:      "killed",
		Class:     provider.Timeout,
		ExitCodes: []int{137},
	},
)

// Classify judges a finished codex run.
//
// Anything unmatched is a task failure — never a quota condition. See provider.Matcher.Classify.
func (p *Provider) Classify(exit int, stdout, stderr string) provider.Classification {
	return matcher.Classify(exit, stdout, stderr)
}
