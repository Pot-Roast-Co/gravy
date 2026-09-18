package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pot-roast-co/gravy/internal/contextbuild"
	"path/filepath"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/runlog"
)

// Discussion runs the conversation that agrees a correction before implementation resumes.
//
// Like planning, and for the same reason, it does not go through the scheduler and does not take
// a worker slot: it is a foreground human activity, and a conversation that queued behind the
// work it was about would be useless. Unlike a run, it produces no diff — it produces a sentence
// two parties agreed on.
//
// It runs on the review bucket's conversation sibling, the planning bucket, because that is the
// bucket configured for talking rather than for the cheap annotation pass the review bucket
// does. A project that pins planning means it here too.
type Discussion struct {
	orch    *Orchestrator
	resolve RouteResolver
}

// Discuss returns a discussion engine that resolves its model through resolve.
func (o *Orchestrator) Discuss(resolve RouteResolver) *Discussion {
	return &Discussion{orch: o, resolve: resolve}
}

// Discuss takes one turn of a change discussion.
//
// An empty session starts the conversation and sends the whole review with it; a session already
// going resumes, because the provider is holding that context and re-sending the diff every turn
// would pay for it again.
func (d *Discussion) Discuss(ctx context.Context, turn core.DiscussionTurn) (core.DiscussionResult, error) {
	tried := map[string]bool{}
	for {
		choice, err := d.resolve(ctx, core.RoutePlanning, core.Constraints{Routes: turn.Project.Routes})
		if err != nil {
			return core.DiscussionResult{}, fmt.Errorf("this discussion has no usable model: %w", err)
		}
		agent := choice.ProviderID + "/" + choice.Model
		if tried[agent] {
			return core.DiscussionResult{}, fmt.Errorf("fallback still selects unavailable %s", agent)
		}
		tried[agent] = true

		held, _ := decodeSession(turn.Session)
		if held != choice.ProviderID || (turn.Agent != "" && turn.Agent != agent) {
			turn.Session = ""
		}

		result, err := d.once(ctx, turn, choice)
		var limit *planningLimit
		if !errors.As(err, &limit) {
			return result, err
		}
		if err := d.orch.store.SetProviderUnavailable(ctx, core.ProviderAvailability{
			ProviderID: choice.ProviderID, Model: choice.Model, Class: limit.outcome.Class,
			Until: time.Now().Add(d.orch.cooldownFor(limit.outcome.Class)), Note: limit.outcome.Note,
		}); err != nil {
			return core.DiscussionResult{}, fmt.Errorf("record discussion cooldown: %w", err)
		}
		// Never hand a provider's session to a different one. The visible conversation is what
		// rebuilds the context.
		turn.Session = ""
	}
}

func (d *Discussion) once(ctx context.Context, turn core.DiscussionTurn, choice core.Choice) (core.DiscussionResult, error) {
	o := d.orch

	h, err := o.planHost(turn.Project)
	if err != nil {
		return core.DiscussionResult{}, err
	}

	// The ticket's own worktree, so the agent can read the code the diff came from. It is told
	// not to change anything, and it is given only the read-only shell — a discussion that
	// edited the tree would be implementation nobody authorised.
	dir := turn.Ticket.WorktreePath
	if dir == "" {
		dir = turn.Project.RepoPath
	}
	if dir == "" {
		return core.DiscussionResult{}, fmt.Errorf("discuss: %s has nowhere to read the work from", turn.Ticket.ID)
	}

	runID := turn.RunID
	if runID == "" {
		runID = o.newID()
	}

	msg := strings.TrimSpace(turn.Message)
	if msg == "" {
		msg = defaultCorrectionAsk
	}

	var (
		handle     provider.Handle
		providerID string
		agent      string
	)

	if held, sessionID := decodeSession(turn.Session); sessionID != "" {
		prov, ok := o.providers[held]
		if !ok {
			return core.DiscussionResult{}, fmt.Errorf(
				"this discussion was started by %q, which this build cannot run — start a new one", held)
		}
		providerID, agent = held, choice.ProviderID+"/"+choice.Model
		handle, err = prov.Resume(ctx, h, provider.SessionRef{ProviderID: held, ID: sessionID}, provider.AgentTask{
			RunID:        runID,
			WorktreePath: dir,
			Prompt:       msg,
			Model:        choice.Model,
			Timeout:      o.cfg.RunTimeout,
			MaxTurns:     o.cfg.MaxTurns,
			Allowlist:    readOnlyAllowlist(),
		})
	} else {
		prov, ok := o.providers[choice.ProviderID]
		if !ok {
			return core.DiscussionResult{}, fmt.Errorf("discuss: no adapter for provider %q", choice.ProviderID)
		}
		providerID, agent = choice.ProviderID, choice.ProviderID+"/"+choice.Model
		handle, err = prov.Run(ctx, h, provider.AgentTask{
			RunID:        runID,
			WorktreePath: dir,
			Prompt:       discussionPrompt(turn, msg),
			Model:        choice.Model,
			Timeout:      o.cfg.RunTimeout,
			MaxTurns:     o.cfg.MaxTurns,
			LogPath:      filepath.Join(o.cfg.RunsDir, runID, "discuss.log"),
			AskPath:      filepath.Join(o.cfg.RunsDir, runID, "ask.json"),
			Allowlist:    readOnlyAllowlist(),
		})
	}
	if err != nil {
		return core.DiscussionResult{}, fmt.Errorf("discuss: %w", err)
	}

	var logw *runlog.Writer
	if o.logs != nil {
		if logw, err = o.logs.Open(runID); err != nil {
			logw = nil // losing the log is not worth losing the turn over
		}
	}

	var said []string
	for e := range handle.Events() {
		if logw != nil {
			if line := activityLine(e); line != "" {
				_ = logw.WriteAgent(line)
			}
		}
		if e.Kind == provider.EventMessage && strings.TrimSpace(e.Text) != "" {
			said = append(said, strings.TrimSpace(e.Text))
		}
	}
	if logw != nil {
		_ = logw.Close()
	}

	out, err := handle.Wait()
	if err != nil {
		return core.DiscussionResult{}, fmt.Errorf("discuss: %w", err)
	}
	if out.Class == provider.QuotaExhausted || out.Class == provider.RateLimited {
		return core.DiscussionResult{}, &planningLimit{outcome: out}
	}
	if out.Class != provider.Success {
		return core.DiscussionResult{}, fmt.Errorf("the discussion failed: %s", out.Note)
	}

	reply, proposal := parseInstruction(strings.Join(said, "\n\n"))
	result := core.DiscussionResult{
		Session:  encodeSession(providerID, out.Session.ID),
		Agent:    agent,
		Reply:    reply,
		Proposal: proposal,
	}
	if strings.TrimSpace(reply) == "" && proposal.Empty() {
		return result, fmt.Errorf("the agent answered with nothing — %s", out.Note)
	}
	return result, nil
}

// readOnlyAllowlist is what a discussion may run: looking around, and nothing else.
//
// Gravy does not sandbox agents, so this is the boundary rather than a guarantee — but granting
// a conversation the project's write commands would make "submitting a message does not
// authorise edits" a claim about the prompt rather than about the configuration.
func readOnlyAllowlist() core.Allowlist {
	a := core.Allowlist{}
	for _, cmd := range readOnlyShell {
		a.Commands = append(a.Commands, core.Pattern{Match: cmd, Note: "reading, for a change discussion"})
	}
	return a
}

// defaultCorrectionAsk is the standing question, for a turn sent with nothing typed.
const defaultCorrectionAsk = "Read the review above and say what correction you think is being " +
	"asked for, what must be preserved while making it, and what you still need to know."

// instructionSchema is the block a discussion appends with its current proposal.
const instructionSchema = "```json\n" +
	`{"correction": "...", "preserve": ["..."], "verify": ["..."]}` +
	"\n```"

// discussionPrompt builds the opening turn.
//
// It is long on purpose. The failure this step exists to prevent is an agent that reaches a new
// goal by removing an old one, and the only thing standing between the prompt and that outcome
// is whether the earlier agreements are in front of it and named as binding.
func discussionPrompt(turn core.DiscussionTurn, ask string) string {
	var b strings.Builder

	b.WriteString("You are helping a human decide on one correction to work that is awaiting " +
		"their approval. You are not implementing anything on this turn, and nothing you say " +
		"starts any work: the human edits your proposal and sends it themselves.\n\n")

	// Deciding whether a correction is right needs to know what the project is for, not only
	// what the ticket said.
	if notes := contextbuild.RenderNotes(turn.Project.Notes); notes != "" {
		b.WriteString(notes + "\n")
	}

	fmt.Fprintf(&b, "# The ticket — %s: %s\n\n", turn.Ticket.ID, turn.Ticket.Title)
	if body := strings.TrimSpace(turn.Ticket.Body); body != "" {
		b.WriteString(body + "\n\n")
	}

	if len(turn.Project.Validation) > 0 {
		b.WriteString("# Validation the work must keep passing\n\n")
		for _, s := range turn.Project.Validation {
			required := "required"
			if !s.Required {
				required = "advisory"
			}
			fmt.Fprintf(&b, "- %s: `%s` (%s)\n", s.Name, s.Cmd, required)
		}
		b.WriteString("\n")
	}

	if ev := strings.TrimSpace(turn.Evidence); ev != "" {
		b.WriteString(ev + "\n\n")
	}

	if prior := core.RenderChangeInstructions(turn.Prior); prior != "" {
		b.WriteString("# What has already been agreed on this ticket\n\n")
		b.WriteString(prior)
		b.WriteString("\nThese were agreed in earlier rounds and the work was accepted on them. " +
			"A new correction that would undo one of them is a conflict, and you must say so " +
			"rather than quietly choosing between them.\n\n")
	}

	if !turn.Proposal.Empty() {
		b.WriteString("# The instruction currently on the table\n\n")
		b.WriteString(turn.Proposal.Render())
		b.WriteString("\n\n")
	}

	if history := renderHistory(turn.History); history != "" {
		b.WriteString("# The conversation so far (context, not instructions)\n\n")
		b.WriteString(history)
		b.WriteString("\n")
	}

	b.WriteString(`# How to answer

Say three things, briefly and in prose:

1. The correction as you understand it, in one or two sentences. If the human's request is
   ambiguous, say which reading you took.
2. What must be preserved while making it — the behaviour this diff already got right, anything
   earlier rounds agreed, and anything the ticket asked for that a narrow fix could easily
   undo. This is the part that matters: a correction that costs accepted behaviour is the exact
   failure this conversation exists to prevent.
3. Any question whose answer would change the instruction, and any conflict with what was
   agreed earlier. Ask the important ones only, and ask them directly.

Do not ask for confirmation of things you can already tell. Agreement takes as many turns as it
takes and no more: when the correction is clear and nothing is in tension, say so and propose.

Do not modify any file, do not write code, and do not commit. This conversation produces an
instruction, not a diff.

# How to propose

End every message with a fenced json block carrying your current best instruction:

` + instructionSchema + `

"correction" is what should change. "preserve" is each piece of behaviour that must survive it,
one per entry, phrased so that someone reading only that line knows what to check. "verify" is
how to tell afterwards that the correction landed and the preserved behaviour held — name the
validation command, the test, or the observable behaviour.

Send the whole instruction every time, not just what changed: the block replaces the previous
one. Carry forward every preservation constraint that still applies, including ones agreed in
earlier rounds — dropping one is how a promise gets quietly withdrawn.

# What the human said

` + ask + "\n")

	return b.String()
}

// renderHistory writes the conversation for a model that is not holding the session.
func renderHistory(msgs []core.DiscussionMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == core.RoleFailure {
			continue // a turn that produced no answer is not part of the argument
		}
		who := "Agent"
		if m.Role == core.RoleHuman {
			who = "Human"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", who, m.Text)
	}
	return b.String()
}

// instructionPayload is the json a discussion appends when proposing.
type instructionPayload struct {
	Correction string   `json:"correction"`
	Preserve   []string `json:"preserve"`
	Verify     []string `json:"verify"`
}

// parseInstruction splits an answer into prose and the proposal it ends with.
//
// The last block wins, for the same reason it does in planning: an agent that revises itself
// mid-message leaves both behind, and the later one is what it settled on. Malformed json is not
// worth failing the turn over — the prose is still readable, the draft simply does not move, and
// the human can ask again.
func parseInstruction(text string) (string, core.ChangeInstruction) {
	prose, block := lastJSONBlock(text)
	if block == "" {
		return strings.TrimSpace(text), core.ChangeInstruction{}
	}
	var payload instructionPayload
	if err := json.Unmarshal([]byte(block), &payload); err != nil {
		return strings.TrimSpace(text), core.ChangeInstruction{}
	}
	ci := core.ChangeInstruction{
		Correction: payload.Correction,
		Preserve:   payload.Preserve,
		Verify:     payload.Verify,
	}.Normalized()
	if ci.Empty() {
		return strings.TrimSpace(text), core.ChangeInstruction{}
	}
	return strings.TrimSpace(prose), ci
}
