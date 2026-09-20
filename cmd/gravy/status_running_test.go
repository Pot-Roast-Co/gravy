package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// TestStatusPrintsWhatEachRunIsDoing is the CLI half of the progress journal: `gravy status` is
// what a human runs from a terminal to ask whether anything needs them, and a RUNNING section
// that lists only state names answers "it is running" for the whole length of a run.
func TestStatusPrintsWhatEachRunIsDoing(t *testing.T) {
	var b strings.Builder
	printRunning(&b, []api.RunningTicket{{
		Ticket:   core.Ticket{ID: "GR-100", State: core.StateValidating},
		Elapsed:  90 * time.Second,
		Activity: "test failed in 42.1s (exit 1)",
	}})

	out := b.String()
	for _, want := range []string{"RUNNING (1)", "GR-100", "validating", "test failed in 42.1s (exit 1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing %q:\n%s", want, out)
		}
	}
}

// TestStatusSaysWhenNothingIsRunning keeps an empty fleet explained rather than blank: a section
// header with nothing under it reads like a bug in the tool.
func TestStatusSaysWhenNothingIsRunning(t *testing.T) {
	var b strings.Builder
	printRunning(&b, nil)
	if out := b.String(); !strings.Contains(out, "RUNNING (0)") || !strings.Contains(out, "nothing in flight") {
		t.Errorf("empty RUNNING section = %q", out)
	}
}
