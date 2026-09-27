package agentrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
)

// note appends one sentence by Gravy, with no payload, to a ticket's history.
func (o *Orchestrator) note(ctx context.Context, ticketID, runID string, kind core.ActivityKind, format string, args ...any) {
	o.record(ctx, core.Activity{
		TicketID: ticketID, RunID: runID, Kind: kind, Detail: fmt.Sprintf(format, args...),
	})
}

// noteWith is note with the facts behind the sentence, for a reader that is a program.
func (o *Orchestrator) noteWith(ctx context.Context, ticketID, runID string, kind core.ActivityKind, payload map[string]any, format string, args ...any) {
	o.record(ctx, core.Activity{
		TicketID: ticketID, RunID: runID, Kind: kind, Payload: payload,
		Detail: fmt.Sprintf(format, args...),
	})
}

// record appends one entry to a ticket's history, filling in its id, its time, and Gravy as the
// actor when the caller named nobody.
//
// Best effort, on purpose. The history narrates the work; it is not the work, and a run that
// could not write its commentary has still done the thing it was commenting on. A failure here
// is logged and the run carries on. For the same reason it only ever writes: an entry never
// changes the ticket and never raises attention.
//
// The write drops the caller's cancellation because the entries that matter most are the last
// ones — "parked in Needs You", "requeued" — and those are written at exactly the moment the
// context has usually just been cancelled by whatever ended the run. A history that goes silent
// precisely when something goes wrong is worse than no history.
func (o *Orchestrator) record(ctx context.Context, a core.Activity) {
	a.ID = o.newID()
	a.At = time.Now()
	if a.Actor == "" {
		a.Actor = core.ActorGravy
	}
	if err := o.store.AddActivity(context.WithoutCancel(ctx), a); err != nil {
		o.log.Warn("could not record history", "ticket", a.TicketID, "kind", a.Kind, "error", err)
	}
}

// noteCommit records a commit of the agent's work, with what it changed.
//
// The counts are the commit's own — against its parent, not against the target — so a retry
// that fixed one line says it fixed one line rather than repeating the whole branch. A diff that
// cannot be read still leaves the commit on record: the hash is the fact that matters.
func (o *Orchestrator) noteCommit(ctx context.Context, ticketID, runID string, repo Repo, wt git.Worktree, hash string) {
	payload := map[string]any{"hash": hash}
	detail := "committed " + shortCommit(hash)
	if d, err := repo.Diff(ctx, wt, hash+"^"); err == nil {
		ins, del := d.Totals()
		payload["files"], payload["insertions"], payload["deletions"] = len(d.Files), ins, del
		detail += fmt.Sprintf(": %d file(s), +%d -%d", len(d.Files), ins, del)
	}
	o.noteWith(ctx, ticketID, runID, core.KindCommit, payload, "%s", detail)
}

// evidence renders a provider's matched evidence as a trailing clause, or nothing when it said
// nothing. A classification without its evidence is the thing that makes a misclassification
// mysterious rather than diagnosable.
func evidence(note string) string {
	if note == "" {
		return ""
	}
	return ": " + flatten(note, 120)
}

// flatten collapses a provider's own words onto one line and caps them, so one entry stays one
// sentence however many lines of output the evidence was quoted from.
func flatten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// shortCommit abbreviates a hash for a sentence a human scans, leaving anything that is not one
// alone.
func shortCommit(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// took renders a duration at a granularity a human cares about.
func took(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// pidHandle is a provider handle that can name the operating-system process behind it.
//
// Optional rather than part of provider.Handle: ARCHITECTURE §4.2 defines that interface, and a
// provider that is not a local subprocess — an HTTP-backed one later — has no pid to give. An
// adapter that can answer does; the journal says so when it can and stays quiet when it cannot.
type pidHandle interface{ PID() int }

// pidOf reports the process behind a handle, or 0 when the provider does not say.
func pidOf(h any) int {
	if p, ok := h.(pidHandle); ok {
		return p.PID()
	}
	return 0
}

// pidNote renders a pid for an entry, or nothing when the provider did not report one.
func pidNote(pid int) string {
	if pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}
