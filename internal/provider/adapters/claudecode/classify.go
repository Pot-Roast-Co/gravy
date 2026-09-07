package claudecode

import (
	"regexp"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// matcher is the classification table for the claude CLI.
//
// Every pattern below is justified by output actually observed from the binary during the GR-000
// spike; the fixtures are in docs/fixtures/claude-code/. Patterns invented from imagination are
// how a classifier drifts into misclassifying, so anything added here should come with a
// captured example.
//
// Order matters: the first match wins, so specific rules precede general ones.
var matcher = provider.NewMatcher(
	// The CLI emits machine-readable tags on stderr alongside the human text, for example:
	//   [claude-code:unrecognized_model] {"model":"no-such-model-9000","query_source":"sdk"}
	// Parsing the tag is far more stable than matching prose, which is reworded freely.
	provider.Rule{
		Name:    "tagged auth error",
		Class:   provider.AuthExpired,
		Pattern: regexp.MustCompile(`\[claude-code:(authentication_error|invalid_api_key|oauth_.*_error)\]`),
	},
	provider.Rule{
		// An unrecognised model is a CONFIGURATION error, not a provider outage.
		//
		// Observed: exit 1, stderr `[claude-code:unrecognized_model] {"model":"..."}`, and a
		// result object with api_error_status 404. Escalating this up the fallback chain
		// would spend the expensive route on a request guaranteed to 404 again, so it is
		// deliberately a task failure.
		Name:    "unrecognized model",
		Class:   provider.TaskFailure,
		Pattern: regexp.MustCompile(`\[claude-code:unrecognized_model\]`),
	},
	provider.Rule{
		// Observed with an invalid ANTHROPIC_API_KEY, after ~188s of silent retries:
		//   "Failed to authenticate. API Error: 401 API key is invalid."
		// with api_error_status 401 in the result object.
		Name:    "http 401 unauthenticated",
		Class:   provider.AuthExpired,
		Pattern: regexp.MustCompile(`(?i)"?api_error_status"?:\s*401|API Error:\s*401|api key is invalid|failed to authenticate`),
	},
	provider.Rule{
		// The CLI reports quota state through rate_limit_event while healthy; an exhausted
		// window surfaces in the result text. Both wordings are matched because the exhausted
		// state could not be induced during the spike and so is unverified.
		Name:    "usage limit reached",
		Class:   provider.QuotaExhausted,
		Pattern: regexp.MustCompile(`(?i)usage limit reached|quota (has been )?exceeded|out of (credits|usage)`),
	},
	provider.Rule{
		Name:    "http 429 rate limited",
		Class:   provider.RateLimited,
		Pattern: regexp.MustCompile(`(?i)"?api_error_status"?:\s*429|API Error:\s*429|rate limit`),
	},
	provider.Rule{
		// 5xx is the provider's side failing, not the agent's work.
		Name:    "upstream unavailable",
		Class:   provider.ProviderUnavailable,
		Pattern: regexp.MustCompile(`(?i)"?api_error_status"?:\s*5\d\d|API Error:\s*5\d\d|overloaded_error`),
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

// Classify judges a finished claude run.
//
// Anything unmatched is a task failure — never a quota condition. See provider.Matcher.Classify.
func (p *Provider) Classify(exit int, stdout, stderr string) provider.Classification {
	return matcher.Classify(exit, stdout, stderr)
}
