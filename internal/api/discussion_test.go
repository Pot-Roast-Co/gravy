package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// stubDiscusser answers with whatever it was told to and records the turn it was handed.
//
// What a model would actually say is not this package's business. What is, is the turn the
// daemon assembles and what it does with the answer, so the stub asserts on the first and
// controls the second.
type stubDiscusser struct {
	mu       sync.Mutex
	turns    []core.DiscussionTurn
	reply    string
	proposal core.ChangeInstruction
	err      error
}

func (s *stubDiscusser) Discuss(_ context.Context, turn core.DiscussionTurn) (core.DiscussionResult, error) {
	s.mu.Lock()
	s.turns = append(s.turns, turn)
	err, reply, proposal := s.err, s.reply, s.proposal
	s.mu.Unlock()
	if err != nil {
		return core.DiscussionResult{}, err
	}
	return core.DiscussionResult{
		Session: "claude-code:sess-1", Agent: "claude-code/sonnet",
		Reply: reply, Proposal: proposal,
	}, nil
}

func (s *stubDiscusser) seen() []core.DiscussionTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.DiscussionTurn(nil), s.turns...)
}

// talking returns a service at review with an agent willing to discuss.
func talking(t *testing.T) (*Local, *store.DB, *stubDiscusser) {
	t.Helper()
	svc, db := atReview(t)
	d := &stubDiscusser{
		reply:    "You want the nil case handled. Multiply's current results must not move.",
		proposal: core.ChangeInstruction{Correction: "Return an error on a nil operand"},
	}
	return svc.WithDiscusser(d), db, d
}

// reopen is the daemon restarting: the same database file, a fresh service, nothing carried
// over in memory.
func reopen(t *testing.T, path string) (*Local, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var n int
	return NewLocal(db, nil, nil, func() string { n++; return fmt.Sprintf("restart-%d", n) }), db
}

// stillAwaitingJudgement asserts the ticket has not moved and carries no new instructions.
func stillAwaitingJudgement(t *testing.T, db *store.DB, ticketID string) {
	t.Helper()
	ctx := context.Background()
	got, err := db.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.StateReview {
		t.Errorf("state = %q, want the ticket left in review", got.State)
	}
	if got.Feedback != "" {
		t.Errorf("feedback = %q, want nothing sent to an agent", got.Feedback)
	}
	open, err := db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Errorf("queue holds %+v, want the review still waiting on a human", open)
	}
	agreed, err := db.ListChangeInstructions(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agreed) != 0 {
		t.Errorf("instructions were recorded without a confirmation: %+v", agreed)
	}
}

// TestOpeningADiscussionAuthorisesNothing is AC1. Request changes used to be a single keystroke
// that put work back in front of an agent; opening the step in front of it must not.
func TestOpeningADiscussionAuthorisesNothing(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()

	view, err := svc.OpenDiscussion(ctx, "GR-1")
	if err != nil {
		t.Fatalf("OpenDiscussion: %v", err)
	}
	if view.Discussion.State != core.DiscussionOpen {
		t.Errorf("state = %q, want an open conversation", view.Discussion.State)
	}
	if len(view.Discussion.Messages) != 0 {
		t.Errorf("a fresh discussion arrived with %+v in it", view.Discussion.Messages)
	}
	stillAwaitingJudgement(t, db, "GR-1")

	// Opening it again resumes the same conversation rather than starting a second one.
	again, err := svc.OpenDiscussion(ctx, "GR-1")
	if err != nil {
		t.Fatalf("second OpenDiscussion: %v", err)
	}
	if again.Discussion.ID != view.Discussion.ID {
		t.Errorf("reopening started a new discussion: %q then %q", view.Discussion.ID, again.Discussion.ID)
	}
}

// TestDiscussionMessagesNeverStartImplementation is AC1's real content, and the invariant the
// whole step rests on: a message is a message.
func TestDiscussionMessagesNeverStartImplementation(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()

	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"the nil case panics", "narrow it to the parser only"} {
		if _, err := svc.Discuss(ctx, DiscussReq{TicketID: "GR-1", Message: msg}); err != nil {
			t.Fatalf("Discuss(%q): %v", msg, err)
		}
	}

	stillAwaitingJudgement(t, db, "GR-1")

	// The conversation itself is recorded, so closing the UI does not lose it.
	cd, err := db.OpenDiscussionFor(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cd.Messages) != 4 {
		t.Errorf("transcript = %+v, want two turns each with a reply", cd.Messages)
	}
	if cd.Proposal.Correction == "" {
		t.Error("the agent's proposal was not kept as the draft")
	}
}

// TestTheDiscussionArguesFromTheReviewTheHumanIsLookingAt is AC2. A conversation about a
// correction to work nobody has put in front of it is a conversation about nothing.
func TestTheDiscussionArguesFromTheReviewTheHumanIsLookingAt(t *testing.T) {
	svc, db, d := talking(t)
	ctx := context.Background()

	if err := db.CreateRun(ctx, core.Run{ID: "run-1", TicketID: "GR-1",
		ProviderID: "claude-code", Model: "sonnet", HostID: "local", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddValidation(ctx, "v1", "run-1", "test", 1, 120, ""); err != nil {
		t.Fatal(err)
	}

	// A first round, already agreed, so the turn has prior instructions to carry.
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendChanges(ctx, SendChangesReq{TicketID: "GR-1", Confirm: true,
		Proposal: core.ChangeInstruction{
			Correction: "Add the table-driven test",
			Preserve:   []string{"Multiply keeps returning 0 for a zero operand"},
		}}); err != nil {
		t.Fatal(err)
	}
	// Back to review, as a re-run would leave it.
	for _, ev := range []core.Event{core.EventAssign, core.EventStart, core.EventAgentFinished,
		core.EventValidationPassed, core.EventReviewed} {
		if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
			t.Fatalf("advance with %s: %v", ev, err)
		}
	}

	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discuss(ctx, DiscussReq{TicketID: "GR-1", Message: "it still panics"}); err != nil {
		t.Fatalf("Discuss: %v", err)
	}

	turns := d.seen()
	if len(turns) != 1 {
		t.Fatalf("the agent saw %d turns, want 1", len(turns))
	}
	turn := turns[0]
	if turn.Ticket.ID != "GR-1" || turn.Ticket.Title == "" {
		t.Errorf("the turn does not carry the original ticket: %+v", turn.Ticket)
	}
	if !strings.Contains(turn.Evidence, "test") || !strings.Contains(turn.Evidence, "FAILED") {
		t.Errorf("the recorded validation is not in the evidence:\n%s", turn.Evidence)
	}
	if len(turn.Prior) != 1 || turn.Prior[0].Correction != "Add the table-driven test" {
		t.Errorf("prior agreed instructions = %+v, want the first round", turn.Prior)
	}
	if turn.Message != "it still panics" {
		t.Errorf("message = %q", turn.Message)
	}
}

// TestSendChangesRefusesWithoutConfirmation is AC3. "The human explicitly confirmed this" is a
// fact the daemon checks, not a convention each client is trusted to follow.
func TestSendChangesRefusesWithoutConfirmation(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}

	err := svc.SendChanges(ctx, SendChangesReq{TicketID: "GR-1",
		Proposal: core.ChangeInstruction{Correction: "handle nil"}})
	if err == nil {
		t.Fatal("an unconfirmed instruction was sent for implementation")
	}
	stillAwaitingJudgement(t, db, "GR-1")

	// And an empty correction is not an instruction at all.
	if err := svc.SendChanges(ctx, SendChangesReq{TicketID: "GR-1", Confirm: true,
		Proposal: core.ChangeInstruction{Preserve: []string{"something"}}}); err == nil {
		t.Fatal("an empty correction was accepted")
	}
	stillAwaitingJudgement(t, db, "GR-1")
}

// TestSendChangesReusesTheRequestChangesLifecycle is AC3 and AC5's tail: the confirmed
// instruction is what moves the ticket, through the path request changes has always used.
func TestSendChangesReusesTheRequestChangesLifecycle(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}

	before, _ := db.GetTicket(ctx, "GR-1")
	instruction := core.ChangeInstruction{
		Correction: "Return an error on a nil operand",
		Preserve:   []string{"Multiply keeps handling negative inputs"},
		Verify:     []string{"go test ./..."},
	}
	if err := svc.SendChanges(ctx, SendChangesReq{
		TicketID: "GR-1", Proposal: instruction, Confirm: true}); err != nil {
		t.Fatalf("SendChanges: %v", err)
	}

	got, err := db.GetTicket(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.StateReady {
		t.Errorf("state = %q, want ready so the agent picks it up again", got.State)
	}
	if got.WorktreePath != before.WorktreePath {
		t.Errorf("worktree moved from %q to %q", before.WorktreePath, got.WorktreePath)
	}
	// Existing readers of the ticket's note — the card, the CLI — still see what was asked for.
	for _, want := range []string{"nil operand", "negative inputs", "go test ./..."} {
		if !strings.Contains(got.Feedback, want) {
			t.Errorf("the ticket's note omits %q: %q", want, got.Feedback)
		}
	}
	open, _ := db.ListOpenAttention(ctx)
	if len(open) != 0 {
		t.Errorf("the queue still holds %+v after the work went back", open)
	}
	cd, err := db.OpenDiscussionFor(ctx, "GR-1")
	if err == nil {
		t.Errorf("the discussion is still open after being sent: %+v", cd)
	}
}

// TestCancellingLeavesTheTicketInReview is AC4. Backing out of a conversation is not a decision.
func TestCancellingLeavesTheTicketInReview(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discuss(ctx, DiscussReq{TicketID: "GR-1", Message: "actually, never mind"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveProposal(ctx, ProposalReq{TicketID: "GR-1",
		Proposal: core.ChangeInstruction{Correction: "something I thought better of"}}); err != nil {
		t.Fatal(err)
	}

	if err := svc.CancelDiscussion(ctx, "GR-1"); err != nil {
		t.Fatalf("CancelDiscussion: %v", err)
	}
	stillAwaitingJudgement(t, db, "GR-1")

	// Cancelling one nobody is having is not a failure either.
	if err := svc.CancelDiscussion(ctx, "GR-1"); err != nil {
		t.Errorf("cancelling twice: %v", err)
	}
}

// TestASecondNarrowCorrectionKeepsTheFirstsPreservationConstraint is the regression this whole
// ticket exists for.
//
// Round one agrees that the retry must not cost a behaviour. Round two is a small, unrelated
// correction. The constraint from round one has to still be in front of the agent implementing
// round two — otherwise the narrow fix is free to reach its goal by removing the earlier one,
// which is exactly what sending feedback straight into execution allowed.
func TestASecondNarrowCorrectionKeepsTheFirstsPreservationConstraint(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()

	first := core.ChangeInstruction{
		Correction: "Return an error on a nil operand instead of panicking",
		Preserve:   []string{"Multiply keeps returning 0 when either operand is 0"},
		Verify:     []string{"go test ./calc"},
	}
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendChanges(ctx, SendChangesReq{
		TicketID: "GR-1", Proposal: first, Confirm: true}); err != nil {
		t.Fatalf("first SendChanges: %v", err)
	}

	// The agent runs again and comes back for judgement.
	for _, ev := range []core.Event{core.EventAssign, core.EventStart, core.EventAgentFinished,
		core.EventValidationPassed, core.EventReviewed} {
		if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
			t.Fatalf("advance with %s: %v", ev, err)
		}
	}

	second := core.ChangeInstruction{
		Correction: "Name the error variable ErrNilOperand",
		Verify:     []string{"go test ./calc"},
	}
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendChanges(ctx, SendChangesReq{
		TicketID: "GR-1", Proposal: second, Confirm: true}); err != nil {
		t.Fatalf("second SendChanges: %v", err)
	}

	agreed, err := db.ListChangeInstructions(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(agreed) != 2 {
		t.Fatalf("recorded %d instructions, want both rounds", len(agreed))
	}
	if agreed[0].Correction != first.Correction || agreed[1].Correction != second.Correction {
		t.Fatalf("instructions are out of order: %+v", agreed)
	}

	// What the next implementation attempt is told.
	prompt := core.RenderChangeInstructions(agreed)
	if !strings.Contains(prompt, second.Correction) {
		t.Errorf("the correction to make is not the latest one:\n%s", prompt)
	}
	if !strings.Contains(prompt, first.Preserve[0]) {
		t.Errorf("the first round's preservation constraint was lost:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Still binding from earlier corrections") {
		t.Errorf("the earlier constraint is not named as binding:\n%s", prompt)
	}
}

// TestAgreedInstructionsSurviveARestart is AC4's other half. A conversation held in memory is a
// conversation lost to the next daemon restart, and with it the record of what was promised.
func TestAgreedInstructionsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravy.db")
	svc, db := atReviewIn(t, path)
	d := &stubDiscusser{reply: "understood",
		proposal: core.ChangeInstruction{Correction: "Return an error on a nil operand"}}
	svc = svc.WithDiscusser(d)
	ctx := context.Background()

	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discuss(ctx, DiscussReq{TicketID: "GR-1", Message: "it panics on nil"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveProposal(ctx, ProposalReq{TicketID: "GR-1",
		Proposal: core.ChangeInstruction{
			Correction: "Return an error on a nil operand",
			Preserve:   []string{"Multiply keeps returning 0 when either operand is 0"},
		}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// A conversation still in progress comes back with its draft and its transcript.
	restarted, _ := reopen(t, path)
	view, err := restarted.OpenDiscussion(ctx, "GR-1")
	if err != nil {
		t.Fatalf("OpenDiscussion after restart: %v", err)
	}
	if len(view.Discussion.Messages) != 2 {
		t.Errorf("transcript after restart = %+v", view.Discussion.Messages)
	}
	if len(view.Discussion.Proposal.Preserve) != 1 {
		t.Errorf("the edited draft did not survive: %+v", view.Discussion.Proposal)
	}

	if err := restarted.SendChanges(ctx, SendChangesReq{
		TicketID: "GR-1", Proposal: view.Discussion.Proposal, Confirm: true}); err != nil {
		t.Fatalf("SendChanges after restart: %v", err)
	}

	// And the agreement outlives a second restart, which is what makes it binding in round two.
	again, reDB := reopen(t, path)
	agreed, err := reDB.ListChangeInstructions(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(agreed) != 1 || len(agreed[0].Preserve) != 1 {
		t.Fatalf("agreed instructions after restart = %+v", agreed)
	}
	if agreed[0].AgreedAt.IsZero() {
		t.Error("the instruction records no time of agreement")
	}
	// The same service a second client would get.
	if _, err := again.OpenDiscussion(ctx, "GR-1"); err == nil {
		t.Error("a sent ticket that is no longer in review opened a new discussion")
	}
}

// TestDirectRequestChangesStillCarriesEarlierAgreements is AC6's compatibility half: a script
// that has always called RequestChanges keeps working, and the constraints agreed in earlier
// rounds are still on the ticket for the next prompt to read.
func TestDirectRequestChangesStillCarriesEarlierAgreements(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()

	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendChanges(ctx, SendChangesReq{TicketID: "GR-1", Confirm: true,
		Proposal: core.ChangeInstruction{
			Correction: "Return an error on a nil operand",
			Preserve:   []string{"Multiply keeps returning 0 when either operand is 0"},
		}}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []core.Event{core.EventAssign, core.EventStart, core.EventAgentFinished,
		core.EventValidationPassed, core.EventReviewed} {
		if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.RequestChanges(ctx, "GR-1", "rename the error variable"); err != nil {
		t.Fatalf("RequestChanges: %v", err)
	}
	got, _ := db.GetTicket(ctx, "GR-1")
	if got.State != core.StateReady || got.Feedback != "rename the error variable" {
		t.Errorf("the direct route changed behaviour: state %q feedback %q", got.State, got.Feedback)
	}
	agreed, _ := db.ListChangeInstructions(ctx, "GR-1")
	if len(agreed) != 1 {
		t.Fatalf("the direct route lost the earlier agreement: %+v", agreed)
	}
	if !strings.Contains(core.RenderChangeInstructions(agreed),
		"Multiply keeps returning 0 when either operand is 0") {
		t.Error("the earlier preservation constraint no longer reaches the prompt")
	}
}

// TestConcurrentDiscussionTurnsDoNotCorruptTheRecord runs under -race.
//
// Two clients on one ticket is ordinary — a TUI and the CLI, or two terminals — and the failure
// mode worth guarding is not a lost message but a transcript and a draft that disagree, which is
// why they are written together.
func TestConcurrentDiscussionTurnsDoNotCorruptTheRecord(t *testing.T) {
	svc, db, _ := talking(t)
	ctx := context.Background()
	if _, err := svc.OpenDiscussion(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, _ = svc.Discuss(ctx, DiscussReq{TicketID: "GR-1",
					Message: fmt.Sprintf("message %d", i)})
			case 1:
				_, _ = svc.SaveProposal(ctx, ProposalReq{TicketID: "GR-1",
					Proposal: core.ChangeInstruction{Correction: fmt.Sprintf("draft %d", i)}})
			default:
				_, _ = svc.OpenDiscussion(ctx, "GR-1")
			}
		}(i)
	}
	wg.Wait()

	// Whatever order they landed in, there is still exactly one open conversation, it is
	// readable, and nothing was authorised by any of it.
	all, err := db.ListDiscussions(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	var open int
	for _, cd := range all {
		if cd.State == core.DiscussionOpen {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d open discussions after concurrent turns, want 1", open)
	}
	stillAwaitingJudgement(t, db, "GR-1")
}
