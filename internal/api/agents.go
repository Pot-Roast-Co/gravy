package api

import "context"

// AgentStatus is one agent CLI and whether it can actually run work.
//
// Installed and authenticated are separate because they fail differently and are fixed
// differently: one is an install, the other is a login. Onboarding asks about both, and
// answering "no agents available" to someone who has claude installed but not logged in sends
// them to reinstall it.
type AgentStatus struct {
	ProviderID string
	// Command is what Gravy will run, so the answer names the thing to check on PATH.
	Command       string
	Installed     bool
	Authenticated bool
	// Detail says what to do rather than merely what is wrong.
	Detail string
}

// Ready reports whether this agent can run work now.
func (a AgentStatus) Ready() bool { return a.Installed && a.Authenticated }

// Detector probes the agent CLIs. Nil on a client that cannot.
//
// A live probe rather than a cached fact: an agent's login expires, and a status read from
// startup would confidently report an agent that stopped working an hour ago.
type Detector interface {
	DetectAgents(ctx context.Context) []AgentStatus
}

// WithDetector lets the service report which agents are usable.
func (l *Local) WithDetector(d Detector) *Local {
	l.detector = d
	return l
}

// DetectAgents reports which agent CLIs are installed and logged in.
//
// Never an error: "we could not tell" is itself the answer onboarding needs to show, and a
// failure here must not stop somebody setting Gravy up.
func (l *Local) DetectAgents(ctx context.Context) []AgentStatus {
	if l.detector == nil {
		return nil
	}
	return l.detector.DetectAgents(ctx)
}
