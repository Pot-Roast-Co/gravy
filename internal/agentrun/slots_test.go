package agentrun

import (
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

// TestARunDrawsOnItsOwnMachinesSlots is the regression: a run on another machine took a slot
// from the local pool, starving local projects, and never counted against its own machine.
func TestARunDrawsOnItsOwnMachinesSlots(t *testing.T) {
	local := host.NewLocal("local", 1)
	remote := host.NewLocal("desktop", 2) // any Host with slots stands in for an ssh machine
	o := New(nil, nil, local, nil, nil, Config{RunTimeout: time.Minute}, func() string { return "x" })
	o.RegisterHost(local)
	o.RegisterHost(remote)

	s := o.slotsFor("desktop")
	if !s.TryClaim() {
		t.Fatal("the remote machine had no free slot")
	}
	if used, _ := local.Slots(); used != 0 {
		t.Errorf("a run on desktop used %d of the local machine's slots", used)
	}
	if used, _ := remote.Slots(); used != 1 {
		t.Errorf("desktop counts %d runs, want 1: its limit could never be enforced", used)
	}
	s.Release()

	if o.slotsFor("local") != Slots(local) {
		t.Error("a local run does not draw on the local pool")
	}
	if o.slotsFor("never-registered") != Slots(local) {
		t.Error("an unknown host should fall back to the shared pool")
	}
}
