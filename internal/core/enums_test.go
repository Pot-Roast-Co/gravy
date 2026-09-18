package core

import "testing"

func TestFailureClassEffective(t *testing.T) {
	tests := []struct {
		in   FailureClass
		want FailureClass
	}{
		{Success, Success},
		{TaskFailure, TaskFailure},
		{QuotaExhausted, QuotaExhausted},
		{RateLimited, RateLimited},
		{ProviderUnavailable, ProviderUnavailable},
		{AuthExpired, AuthExpired},
		{Timeout, Timeout},
		// The invariant: an unrecognised failure is the agent having a bad run, never a
		// provider being down. Getting this backwards silently escalates work to the most
		// expensive model.
		{Unknown, TaskFailure},
	}
	for _, tt := range tests {
		t.Run(tt.in.String(), func(t *testing.T) {
			if got := tt.in.Effective(); got != tt.want {
				t.Errorf("%v.Effective() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestUnknownIsNeverAQuotaCondition is the same invariant from the routing side: an unknown
// error must never cool down a model or advance the fallback chain.
func TestUnknownIsNeverAQuotaCondition(t *testing.T) {
	if Unknown.IsQuotaCondition() {
		t.Error("Unknown classified as a quota condition; it must classify as a task failure")
	}
	if Unknown.Effective().IsQuotaCondition() {
		t.Error("Unknown resolves to a quota condition after Effective()")
	}
	if TaskFailure.IsQuotaCondition() {
		t.Error("TaskFailure classified as a quota condition")
	}
	for _, c := range []FailureClass{QuotaExhausted, RateLimited, ProviderUnavailable, AuthExpired} {
		if !c.IsQuotaCondition() {
			t.Errorf("%v is not classified as a quota condition", c)
		}
	}
	if Success.IsQuotaCondition() || Timeout.IsQuotaCondition() {
		t.Error("Success or Timeout classified as a quota condition")
	}
}

func TestFailureClassString(t *testing.T) {
	want := map[FailureClass]string{
		Success: "success", TaskFailure: "task_failure", QuotaExhausted: "quota_exhausted",
		RateLimited: "rate_limited", ProviderUnavailable: "provider_unavailable",
		AuthExpired: "auth_expired", Timeout: "timeout", Unknown: "unknown",
		FailureClass(99): "invalid",
	}
	for c, s := range want {
		if got := c.String(); got != s {
			t.Errorf("FailureClass(%d).String() = %q, want %q", int(c), got, s)
		}
	}
	// String must be injective, or logs become ambiguous.
	seen := map[string]FailureClass{}
	for c := Success; c <= Unknown; c++ {
		if prev, dup := seen[c.String()]; dup {
			t.Errorf("%v and %v share the string %q", prev, c, c.String())
		}
		seen[c.String()] = c
	}
}

func TestEnumValidity(t *testing.T) {
	t.Run("attention reasons", func(t *testing.T) {
		// PRODUCT.md §8 defines exactly nine reasons.
		if len(AllAttentionReasons) != 9 {
			t.Errorf("got %d attention reasons, want 9", len(AllAttentionReasons))
		}
		for _, r := range AllAttentionReasons {
			if !r.Valid() {
				t.Errorf("%q is not Valid", r)
			}
		}
		if AttentionReason("nope").Valid() {
			t.Error("unknown reason reported Valid")
		}
	})

	t.Run("routes", func(t *testing.T) {
		if len(AllRoutes) != 7 {
			t.Errorf("got %d routes, want 7", len(AllRoutes))
		}
		for _, r := range AllRoutes {
			if !r.IsDefault() {
				t.Errorf("%q is missing from AllRoutes", r)
			}
			if !r.Named() {
				t.Errorf("%q is not a usable bucket name", r)
			}
		}
		// A name Gravy does not ship with is still a usable bucket: routes are named by
		// whoever is using them, and validity is a question about a configuration.
		if custom := Route("astra"); custom.IsDefault() {
			t.Error("a custom bucket name was reported as a default")
		} else if !custom.Named() {
			t.Error("a custom bucket name was rejected")
		}
		for _, bad := range []Route{"", " ", "two words", "claude/opus", "a,b", "a:b"} {
			if bad.Named() {
				t.Errorf("%q was accepted as a bucket name", bad)
			}
		}
	})

	t.Run("land modes", func(t *testing.T) {
		if !LandMerge.Valid() || !LandPR.Valid() {
			t.Error("a known land mode is not Valid")
		}
		if LandMode("nope").Valid() {
			t.Error("unknown land mode reported Valid")
		}
	})

	t.Run("states", func(t *testing.T) {
		if len(AllStates) != 14 {
			t.Errorf("got %d states, want 14", len(AllStates))
		}
		for _, s := range AllStates {
			if !s.Valid() {
				t.Errorf("%q is not Valid", s)
			}
		}
		if State("nope").Valid() {
			t.Error("unknown state reported Valid")
		}
	})
}

// TestNoDuplicateEnumValues guards against a copy-paste collision in the string constants, which
// would make two distinct concepts compare equal.
func TestNoDuplicateEnumValues(t *testing.T) {
	t.Run("states", func(t *testing.T) {
		seen := map[State]bool{}
		for _, s := range AllStates {
			if seen[s] {
				t.Errorf("duplicate state %q", s)
			}
			seen[s] = true
		}
	})
	t.Run("reasons", func(t *testing.T) {
		seen := map[AttentionReason]bool{}
		for _, r := range AllAttentionReasons {
			if seen[r] {
				t.Errorf("duplicate reason %q", r)
			}
			seen[r] = true
		}
	})
	t.Run("routes", func(t *testing.T) {
		seen := map[Route]bool{}
		for _, r := range AllRoutes {
			if seen[r] {
				t.Errorf("duplicate route %q", r)
			}
			seen[r] = true
		}
	})
}
