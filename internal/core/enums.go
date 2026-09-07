package core

import "strings"

// AttentionReason is why an item sits in the Needs You queue (PRODUCT.md §8).
//
// Needs You is the primary queue and, in practice, the product: a single typed list of
// everything awaiting human judgement. If something is not here, Gravy does not need you.
type AttentionReason string

// The attention reasons. M0 ships review_pending, validation_failed, merge_conflict and
// host_unavailable; the rest arrive with their owning features in M1.
const (
	ReasonReviewPending    AttentionReason = "review_pending"
	ReasonAgentQuestion    AttentionReason = "agent_question"
	ReasonPermissionReq    AttentionReason = "permission_request"
	ReasonValidationFailed AttentionReason = "validation_failed"
	ReasonMergeConflict    AttentionReason = "merge_conflict"
	ReasonProviderAuth     AttentionReason = "provider_auth"
	ReasonTicketCritique   AttentionReason = "ticket_critique"
	ReasonHostUnavailable  AttentionReason = "host_unavailable"
)

// AllAttentionReasons lists every reason, in the order PRODUCT.md §8 presents them.
var AllAttentionReasons = []AttentionReason{
	ReasonReviewPending, ReasonAgentQuestion, ReasonPermissionReq, ReasonValidationFailed,
	ReasonMergeConflict, ReasonProviderAuth, ReasonTicketCritique, ReasonHostUnavailable,
}

// Valid reports whether r is a known attention reason.
func (r AttentionReason) Valid() bool {
	for _, k := range AllAttentionReasons {
		if k == r {
			return true
		}
	}
	return false
}

// LandMode is how approved work reaches the target branch.
type LandMode string

// The land modes.
const (
	LandMerge LandMode = "merge" // squash into target, push
	LandPR    LandMode = "pr"    // push branch, then `gh pr create`
)

// Valid reports whether m is a known land mode.
func (m LandMode) Valid() bool { return m == LandMerge || m == LandPR }

// Route is a bucket of agent capacity that a ticket asks for by name.
//
// Tickets request a route; configuration maps each route to an ordered list of provider/model
// choices, and to a concurrency cap. That indirection is what lets quota failures fall through to
// the next choice without a ticket ever naming a model.
//
// Route names are NOT a closed set. AllRoutes below is the set Gravy ships with, used to seed a
// new config and nothing else — a route is valid if the configuration defines it, so you can call
// your buckets whatever you actually call them.
type Route string

// The routes.
const (
	RouteLocal          Route = "local"
	RouteCheap          Route = "cheap"
	RouteStandard       Route = "standard"
	RouteStrong         Route = "strong"
	RoutePlanning       Route = "planning"
	RouteImplementation Route = "implementation"
	RouteReview         Route = "review"
)

// AllRoutes lists the routes a fresh configuration starts with. It is a default, not a
// constraint: see Route.
var AllRoutes = []Route{
	RouteLocal, RouteCheap, RouteStandard, RouteStrong, RoutePlanning,
	RouteImplementation, RouteReview,
}

// IsDefault reports whether r is one of the routes Gravy ships with.
//
// It is deliberately not called Valid: validity is a question about a configuration, not about
// this list, and answering it here is what stopped anyone naming their own buckets.
func (r Route) IsDefault() bool {
	for _, k := range AllRoutes {
		if k == r {
			return true
		}
	}
	return false
}

// Named reports whether r is a usable route name: non-empty, and free of the characters that
// would make it ambiguous in a config file or on a command line.
func (r Route) Named() bool {
	s := strings.TrimSpace(string(r))
	if s == "" || s != string(r) {
		return false
	}
	return !strings.ContainsAny(s, "/,: \t\n")
}

// FailureClass is how a finished run is judged (ARCHITECTURE.md §4.3).
type FailureClass int

// The failure classes.
const (
	// Success is a run that did what it was asked.
	Success FailureClass = iota
	// TaskFailure is the agent failing at the work — retryable by self-correction.
	TaskFailure
	// QuotaExhausted is a genuinely exhausted subscription or quota — cooldown the model.
	QuotaExhausted
	// RateLimited is a transient rate limit — short cooldown.
	RateLimited
	// ProviderUnavailable is a broken CLI or a provider outage.
	ProviderUnavailable
	// AuthExpired needs human re-authentication and escalates immediately.
	AuthExpired
	// Timeout is a wall-clock or turn-cap overrun.
	Timeout
	// Unknown is an unrecognised failure. See Effective.
	Unknown
)

// String renders the class for logs and the TUI.
func (c FailureClass) String() string {
	switch c {
	case Success:
		return "success"
	case TaskFailure:
		return "task_failure"
	case QuotaExhausted:
		return "quota_exhausted"
	case RateLimited:
		return "rate_limited"
	case ProviderUnavailable:
		return "provider_unavailable"
	case AuthExpired:
		return "auth_expired"
	case Timeout:
		return "timeout"
	case Unknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// Effective resolves Unknown to TaskFailure, and is what callers should route on.
//
// This is a standing invariant, not a convenience. Classification is string and exit-code
// matching against CLI output that changes on the vendor's schedule, so misclassification is a
// question of when. A false QuotaExhausted silently escalates work up the fallback chain toward
// the most expensive model — the exact outcome routing exists to prevent. Failing toward "the
// agent had a bad run" is cheap and visible; failing toward "the provider is down" is expensive
// and silent.
func (c FailureClass) Effective() FailureClass {
	if c == Unknown {
		return TaskFailure
	}
	return c
}

// IsQuotaCondition reports whether the class is a genuine provider-side availability problem,
// meaning the model should be cooled down and the route re-resolved.
//
// Unknown is deliberately not a quota condition; see Effective.
func (c FailureClass) IsQuotaCondition() bool {
	switch c {
	case QuotaExhausted, RateLimited, ProviderUnavailable, AuthExpired:
		return true
	default:
		return false
	}
}
