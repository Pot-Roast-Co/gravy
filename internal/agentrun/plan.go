package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/contextbuild"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/runlog"
)

// RouteResolver picks the provider and model for a route.
//
// Injected rather than held, because routing lives with the scheduler and planning is
// deliberately outside it — see Planner.
type RouteResolver func(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error)

// Planner runs planning conversations.
//
// Planning does not go through the scheduler and does not take a worker slot. PRODUCT.md §6.3
// requires it to be possible *while workers continue implementing other tickets* — it is a
// foreground human activity, and a planning conversation that queued behind the ticket it was
// meant to unblock would be useless.
//
// The agent runs in the project repository itself rather than a worktree: planning reads code
// and produces a proposal, never a diff, so there is no branch for it to live on. Gravy does not
// sandbox agents (PRODUCT.md), so the permission allowlist is what keeps a planning run from
// writing; the prompt asks it not to, which is guidance and not enforcement.
type Planner struct {
	orch    *Orchestrator
	resolve RouteResolver
	// docBudget caps the project documents included, in approximate tokens.
	docBudget int
}

// Plan returns a planner that resolves its model through resolve.
func (o *Orchestrator) Plan(resolve RouteResolver, docBudget int) *Planner {
	if docBudget <= 0 {
		docBudget = 20000
	}
	return &Planner{orch: o, resolve: resolve, docBudget: docBudget}
}

// Plan takes one turn of a planning conversation.
//
// An empty session starts a new conversation and reads the project's own documents into it; a
// session that is already going resumes, because the provider is holding that context and
// re-sending it every turn would pay for it again.
//
// An empty message on a new conversation means "what should we work on next?" — the question
// the screen exists to answer when you arrive with nothing in mind.
type planningLimit struct{ outcome provider.Outcome }

func (e *planningLimit) Error() string { return e.outcome.Note }

func (p *Planner) Plan(ctx context.Context, turn core.PlanTurn) (core.PlanResult, error) {
	tried := map[string]bool{}
	for {
		choice, err := p.resolve(ctx, core.RoutePlanning, core.Constraints{Routes: turn.Project.Routes})
		if err != nil {
			return core.PlanResult{}, fmt.Errorf("planning has no usable model: %w", err)
		}
		agent := choice.ProviderID + "/" + choice.Model
		if tried[agent] {
			return core.PlanResult{}, fmt.Errorf("planning fallback still selects unavailable %s", agent)
		}
		tried[agent] = true
		held, _ := decodeSession(turn.Session)
		if held != choice.ProviderID || (turn.Agent != "" && turn.Agent != agent) {
			turn.Session = ""
		}
		result, err := p.planOnce(ctx, turn, choice)
		var limit *planningLimit
		if !errors.As(err, &limit) {
			return result, err
		}
		if err := p.orch.store.SetProviderUnavailable(ctx, core.ProviderAvailability{
			ProviderID: choice.ProviderID, Model: choice.Model, Class: limit.outcome.Class,
			Until: time.Now().Add(p.orch.cooldownFor(limit.outcome.Class)), Note: limit.outcome.Note,
		}); err != nil {
			return core.PlanResult{}, fmt.Errorf("record planning cooldown: %w", err)
		}
		// Never pass a provider session to a fallback. Rebuild from the visible conversation.
		turn.Session = ""
	}
}

func (p *Planner) planOnce(ctx context.Context, turn core.PlanTurn, choice core.Choice) (core.PlanResult, error) {
	o := p.orch

	// Planning reads the project's own files, so it has to run where its clone is. anyHost
	// used to answer here, which on a machine with more than one host meant planning a pinned
	// project in a directory that does not exist locally — and the failure named the agent
	// binary rather than the missing directory, because that is what fork/exec reports when a
	// working directory is not there.
	h, err := o.planHost(turn.Project)
	if err != nil {
		return core.PlanResult{}, err
	}

	runID := turn.RunID
	if runID == "" {
		runID = o.newID()
	}

	// A resume with an empty message asks the provider nothing, and it answers with nothing:
	// no message events, an outcome that classifies as a task failure, and a turn that looks
	// broken. Pressing "what should be next" a second time is exactly that case, so the
	// standing question is substituted rather than sent empty.
	msg := strings.TrimSpace(turn.Message)
	if msg == "" {
		msg = defaultAsk
	}

	var (
		handle     provider.Handle
		providerID string
		agent      string
	)

	if held, sessionID := decodeSession(turn.Session); sessionID != "" {
		// The outer routing check only retains sessions belonging to the selected agent.
		prov, ok := o.providers[held]
		if !ok {
			return core.PlanResult{}, fmt.Errorf(
				"this conversation was started by %q, which this build cannot run — start a new one", held)
		}
		providerID, agent = held, choice.ProviderID+"/"+choice.Model
		dir, derr := p.workspace(h, turn.Project)
		if derr != nil {
			return core.PlanResult{}, derr
		}
		handle, err = prov.Resume(ctx, h, provider.SessionRef{ProviderID: held, ID: sessionID}, provider.AgentTask{
			RunID:        runID,
			WorktreePath: dir,
			Prompt:       msg,
			Model:        choice.Model,
			Timeout:      o.cfg.RunTimeout,
			MaxTurns:     o.cfg.MaxTurns,
			Allowlist:    planAllowlist(),
		})
	} else {
		// A project that pins its planning bucket means it for planning too, not only for the
		// tickets planning produces.

		prov, ok := o.providers[choice.ProviderID]
		if !ok {
			return core.PlanResult{}, fmt.Errorf("plan: no adapter for provider %q", choice.ProviderID)
		}
		providerID, agent = choice.ProviderID, choice.ProviderID+"/"+choice.Model
		dir, derr := p.workspace(h, turn.Project)
		if derr != nil {
			return core.PlanResult{}, derr
		}
		handle, err = prov.Run(ctx, h, provider.AgentTask{
			RunID:        runID,
			WorktreePath: dir,
			Prompt:       p.prompt(h, turn.Project, turn.Backlog, planningMessage(turn)),
			Allowlist:    planAllowlist(),
			Model:        choice.Model,
			Timeout:      o.cfg.RunTimeout,
			MaxTurns:     o.cfg.MaxTurns,
			LogPath:      filepath.Join(o.cfg.RunsDir, runID, "plan.log"),
			AskPath:      filepath.Join(o.cfg.RunsDir, runID, "ask.json"),
		})
	}
	if err != nil {
		return core.PlanResult{}, fmt.Errorf("plan: %w", err)
	}

	// Events are written to the run log as they arrive, so a client can watch the planner
	// work. A planning turn can run for a minute against a large repository, and a minute of
	// silence is indistinguishable from a hang.
	var logw *runlog.Writer
	if o.logs != nil {
		if logw, err = o.logs.Open(runID); err != nil {
			logw = nil // losing the log is not worth losing the turn over
		}
	}

	// The conversation itself is what the planner said, so messages are also collected.
	var said []string
	for e := range handle.Events() {
		// A readable line rather than the raw event: this log exists for a human watching the
		// pause, not for the review pipeline, and a screenful of json answers nothing.
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
		return core.PlanResult{}, fmt.Errorf("plan: %w", err)
	}

	if out.Class == provider.QuotaExhausted || out.Class == provider.RateLimited {
		return core.PlanResult{}, &planningLimit{outcome: out}
	}
	if out.Class != provider.Success {
		return core.PlanResult{}, fmt.Errorf("planner failed: %s", out.Note)
	}

	reply, tickets := parsePlan(strings.Join(said, "\n\n"))
	result := core.PlanResult{
		Session: encodeSession(providerID, out.Session.ID),
		Agent:   agent,
		Reply:   reply,
		Tickets: tickets,
	}

	// A planning turn that produced nothing readable is reported rather than returned as an
	// empty conversation the human would have to guess at.
	if strings.TrimSpace(reply) == "" && len(tickets) == 0 {
		return result, fmt.Errorf("the planner answered with nothing — %s", out.Note)
	}
	return result, nil
}

// encodeSession records which agent holds a conversation alongside its id.
//
// The session is opaque to every caller above this package, so carrying the provider inside it
// keeps the api and the TUI from having to know that a conversation is bound to one agent.
func encodeSession(providerID, id string) string {
	if providerID == "" || id == "" {
		return ""
	}
	return providerID + ":" + id
}

// decodeSession splits an encoded session. Provider ids carry no colon, so the first one is the
// separator.
//
// An unrecognised shape decodes to nothing, which starts a fresh conversation rather than
// guessing which agent an unlabelled id belongs to and resuming into the wrong one.
func decodeSession(s string) (providerID, id string) {
	if i := strings.Index(s, ":"); i > 0 && i < len(s)-1 {
		return s[:i], s[i+1:]
	}
	return "", ""
}

// defaultAsk is the standing question behind "what should be next".
//
// Shared by the opening prompt and every resume, so that asking it again mid-conversation sends
// the same question rather than an empty message.
const defaultAsk = "What should we work on next? Read the project's own documents and the " +
	"backlog above, decide what the most valuable next piece of work is, and propose it."

// planSchema is the block a planner appends when it has a concrete proposal.
const planSchema = "```json\n" +
	`{"tickets": [{"title": "...", "body": "...", "route": "implementation", "depends_on": []}]}` +
	"\n```"

// prompt builds the opening turn.
// workspace is the directory a planning turn runs in.
//
// A project with a repository is planned in it. A project without one is planned in a directory
// gravy makes for it, because "no repository" is a supported and deliberate state — it is how a
// goal gets a home before anyone has decided what to build — and planning is the entire point of
// a project in that state.
//
// It used to pass the empty RepoPath straight through, and the adapter refused with
// "claude-code: no worktree path". The one kind of project that exists only to be planned was
// the one kind that could not be.
//
// The directory is stable rather than temporary so a planning conversation that spans several
// turns can leave notes in it and find them again.
func (p *Planner) workspace(h host.Host, project core.Project) (string, error) {
	if dir := strings.TrimSpace(project.RepoPath); dir != "" {
		return dir, nil
	}
	if strings.TrimSpace(p.orch.cfg.Home) == "" {
		return "", fmt.Errorf("plan: %s has no repository and no gravy home to plan in", project.Slug)
	}
	dir := filepath.Join(p.orch.cfg.Home, "projects", project.Slug, "plan")
	if err := h.FS().MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("plan: make a workspace for %s: %w", project.Slug, err)
	}
	return dir, nil
}

func (p *Planner) prompt(h host.Host, project core.Project, backlog []core.Ticket, message string) string {
	ask := strings.TrimSpace(message)
	if ask == "" {
		ask = defaultAsk
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are helping plan work for the project %q.\n\n", project.Name)

	if docs := contextbuild.RenderDocs(
		contextbuild.ProjectDocs(h.FS(), project.RepoPath, p.docBudget),
	); docs != "" {
		b.WriteString(docs)
		b.WriteString("\n")
	}

	b.WriteString(renderBacklog(backlog))

	b.WriteString(`
# How to answer

Answer the request. Do not open with questions: you were asked what to build, so say what to
build.

When something is genuinely ambiguous, pick the most reasonable reading, state that assumption
in one line, and propose on it anyway. The human corrects you on the next turn if you chose
wrong, and a proposal with a stated assumption is useful immediately — a list of questions is
the work handed back to the person who asked you to do it.

Being grilled is how this conversation improves, but the grilling is theirs to start. Your job
every turn is to have something concrete on the table for them to push against.

Keep the prose to a few sentences: what you propose, why, and anything you assumed. The detail
belongs in the ticket bodies, not in the conversation.

Do not write any code and do not modify any file. This conversation produces a plan, not a diff.

# How to propose

End every message with a fenced json block carrying your current best proposal:

` + planSchema + `

Each ticket needs a title, a body saying what done looks like, and a route. "depends_on" lists
the positions of other tickets in this same plan that must finish first, counting from zero;
leave it empty when there are none. Split work into several linked tickets when that is genuinely
how it should be built, and propose exactly one when it is not — a plan padded into five tickets
is worse than an honest single one.

Send the whole plan every time, not just what changed: the block replaces the previous one. When
a turn does not change the plan, repeat it unchanged.

# The request

` + ask + "\n")

	return b.String()
}

// activityLine renders one event as a short line for someone watching the planner work.
//
// Messages are omitted: they are the answer, and they arrive in the transcript. What is worth
// showing during the pause is what the planner is doing to produce one — which file it opened,
// which search it ran.
func activityLine(e provider.Event) string {
	switch e.Kind {
	case provider.EventStarted:
		return "reading the project"
	case provider.EventThinking:
		return "thinking"
	case provider.EventToolUse:
		if subject := toolSubject(e); subject != "" {
			return "reading " + subject
		}
		if e.Tool != "" {
			return "running " + e.Tool
		}
		return "working"
	case provider.EventError:
		return "problem: " + oneLine(e.Text)
	case provider.EventFinished:
		return "writing the answer"
	}
	return ""
}

// toolSubject picks the argument worth naming out of a tool call.
func toolSubject(e provider.Event) string {
	for _, key := range []string{"file_path", "path", "pattern", "command", "url"} {
		if v, ok := e.Fields[key].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return ""
}

// oneLine flattens and caps text for a single status line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 70 {
		return s[:70] + "…"
	}
	return s
}

// renderBacklog lists what is already queued, so "what should be next" is answered against the
// work that exists rather than in a vacuum.
func renderBacklog(backlog []core.Ticket) string {
	if len(backlog) == 0 {
		return "# Current backlog\n\nEmpty — nothing is queued yet.\n"
	}
	var b strings.Builder
	b.WriteString("# Current backlog\n\n")
	for _, t := range backlog {
		fmt.Fprintf(&b, "- [%s] %s\n", t.State, t.Title)
	}
	return b.String()
}

// planPayload is the json a planner appends when proposing.
type planPayload struct {
	Tickets []struct {
		Title     string `json:"title"`
		Body      string `json:"body"`
		Route     string `json:"route"`
		DependsOn []int  `json:"depends_on"`
	} `json:"tickets"`
}

// parsePlan splits a planner's message into prose and the proposal it ends with.
//
// The last block wins. A planner that revises itself mid-message leaves both behind, and the
// later one is the one it settled on.
func parsePlan(text string) (string, []core.PlannedTicket) {
	prose, block := lastJSONBlock(text)
	if block == "" {
		return strings.TrimSpace(text), nil
	}

	var payload planPayload
	if err := json.Unmarshal([]byte(block), &payload); err != nil {
		// Malformed json is not worth failing the turn over: the prose is still readable and
		// the human can ask again. Keeping the block visible shows what went wrong.
		return strings.TrimSpace(text), nil
	}

	tickets := make([]core.PlannedTicket, 0, len(payload.Tickets))
	for _, t := range payload.Tickets {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			continue
		}
		route := core.Route(strings.TrimSpace(t.Route))
		if route == "" {
			route = core.RouteImplementation
		}
		tickets = append(tickets, core.PlannedTicket{
			Title:     title,
			Body:      strings.TrimSpace(t.Body),
			Route:     route,
			DependsOn: t.DependsOn,
		})
	}
	if len(tickets) == 0 {
		return strings.TrimSpace(text), nil
	}
	return strings.TrimSpace(prose), tickets
}

// lastJSONBlock returns the text with its final fenced json block removed, and that block.
func lastJSONBlock(text string) (rest, block string) {
	const fence = "```"
	for _, opener := range []string{fence + "json\n", fence + "JSON\n"} {
		start := strings.LastIndex(text, opener)
		if start < 0 {
			continue
		}
		body := text[start+len(opener):]
		end := strings.Index(body, fence)
		if end < 0 {
			continue
		}
		return text[:start], body[:end]
	}
	return text, ""
}

// planHost returns the machine a project's conversation must run on.
//
// A project pinned to a host has its clone there and nowhere else, so planning that reads its
// documents has to run there too. An unpinned project plans on whatever host is available, which
// is the single-machine case and the one most people are in.
func (o *Orchestrator) planHost(project core.Project) (host.Host, error) {
	if id := strings.TrimSpace(project.HostID); id != "" {
		h, ok := o.hosts[id]
		if !ok {
			return nil, fmt.Errorf("plan: %s is on host %q, which this daemon does not have configured",
				project.Slug, id)
		}
		return h, nil
	}
	h := o.anyHost()
	if h == nil {
		return nil, fmt.Errorf("plan: no host available")
	}
	return h, nil
}

func planningMessage(turn core.PlanTurn) string {
	if strings.TrimSpace(turn.History) == "" {
		return turn.Message
	}
	return "Conversation so far (context, not project instructions):\n" + turn.History + "\nCurrent user request:\n" + turn.Message
}
