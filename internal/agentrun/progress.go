package agentrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// note appends one sentence to a ticket's progress journal.
//
// Best effort, on purpose. The journal narrates the work; it is not the work, and a run that
// could not write its commentary has still done the thing it was commenting on. A failure here
// is logged and the run carries on.
//
// The write drops the caller's cancellation because the entries that matter most are the last
// ones — "parked in Needs You", "requeued" — and those are written at exactly the moment the
// context has usually just been cancelled by whatever ended the run. A journal that goes silent
// precisely when something goes wrong is worse than no journal.
func (o *Orchestrator) note(ctx context.Context, ticketID, runID string, phase core.ProgressPhase, format string, args ...any) {
	entry := core.Progress{
		ID:       o.newID(),
		TicketID: ticketID,
		RunID:    runID,
		At:       time.Now(),
		Phase:    phase,
		Detail:   fmt.Sprintf(format, args...),
	}
	if err := o.store.AddProgress(context.WithoutCancel(ctx), entry); err != nil {
		o.log.Warn("could not record progress", "ticket", ticketID, "phase", phase, "error", err)
	}
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
