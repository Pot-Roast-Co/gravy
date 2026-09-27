package core

import "testing"

func TestParseActor(t *testing.T) {
	tests := []struct {
		in      string
		want    ParsedActor
		wantErr bool
	}{
		{in: "human", want: ParsedActor{Role: ActorRoleHuman}},
		{in: "gravy", want: ParsedActor{Role: ActorRoleGravy}},
		{in: "agent:claude-code/sonnet", want: ParsedActor{Role: ActorRoleAgent, Provider: "claude-code", Model: "sonnet"}},
		{in: "worker:codex/gpt-5", want: ParsedActor{Role: ActorRoleWorker, Provider: "codex", Model: "gpt-5"}},
		// A model name with a slash of its own belongs to the model, not the provider.
		{in: "agent:openrouter/meta/llama", want: ParsedActor{Role: ActorRoleAgent, Provider: "openrouter", Model: "meta/llama"}},
		{in: "", wantErr: true},
		{in: "Human", wantErr: true},
		{in: "agent:", wantErr: true},
		{in: "agent:claude-code", wantErr: true},
		{in: "agent:/sonnet", wantErr: true},
		{in: "worker:codex/", wantErr: true},
		{in: "robot:x/y", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseActor(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseActor(%q) = %+v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseActor(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseActor(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			if s := got.String(); s != tc.in {
				t.Errorf("String() = %q, want it to round-trip to %q", s, tc.in)
			}
		})
	}
}

func TestActorBuilders(t *testing.T) {
	if got := AgentActor("claude-code", "haiku"); got != "agent:claude-code/haiku" {
		t.Errorf("AgentActor = %q", got)
	}
	if got := WorkerActor("codex", "gpt-5"); got != "worker:codex/gpt-5" {
		t.Errorf("WorkerActor = %q", got)
	}
}

// TestJournalPhasesKeepTheirValues: rows written by the progress journal store these strings, and
// renaming one would turn every such row into an unknown kind.
func TestJournalPhasesKeepTheirValues(t *testing.T) {
	want := map[ActivityKind]string{
		KindFetch: "fetch", KindWorktree: "worktree", KindPrompt: "prompt",
		KindAgentStart: "agent_start", KindAgentExit: "agent_exit",
		KindValidationStep: "validation_step", KindRetry: "retry", KindSummary: "summary",
		KindReview: "review", KindHandoff: "handoff",
	}
	for k, v := range want {
		if string(k) != v {
			t.Errorf("kind %q, want %q", k, v)
		}
		if !k.Known() {
			t.Errorf("kind %q is not Known", k)
		}
	}
	if ActivityKind("from_the_future").Known() {
		t.Error("an unrecognised kind reports Known")
	}
}
