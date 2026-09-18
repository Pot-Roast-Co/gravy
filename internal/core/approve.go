package core

// Approval is what a human chose an approval to do.
//
// Approving is one decision with three outcomes, and they differ in how far gravy carries the
// work rather than in whether the work was accepted. All three require the same gate: rebased
// onto the current target and green against it.
type Approval string

const (
	// ApprovePush squashes onto the target branch and pushes it. The default, and what
	// approving has always meant.
	ApprovePush Approval = "push"
	// ApproveLocal squashes onto the target branch and stops. The merge is real and dependent
	// tickets can build on it; nothing reaches the remote until a human pushes.
	ApproveLocal Approval = "local"
	// ApproveHandOff merges nothing. The branch is validated, rebased and left with its
	// worktree for a human to merge however they like.
	//
	// It frees the project's queue but deliberately does not satisfy dependencies: a dependent
	// ticket branches from the target, so starting one against work that was never merged
	// would build on code that is not there.
	ApproveHandOff Approval = "hand_off"
)

// Valid reports whether a is a known approval.
func (a Approval) Valid() bool {
	switch a {
	case ApprovePush, ApproveLocal, ApproveHandOff:
		return true
	default:
		return false
	}
}

// Merges reports whether this approval puts the work on the target branch.
func (a Approval) Merges() bool { return a == ApprovePush || a == ApproveLocal }

// Pushes reports whether this approval sends the target branch to the remote.
func (a Approval) Pushes() bool { return a == ApprovePush }

// OrDefault returns a, or ApprovePush when it is empty.
//
// Empty is what an older client sends, and what a zero value is. Both mean "approve" as it has
// always behaved.
func (a Approval) OrDefault() Approval {
	if a == "" {
		return ApprovePush
	}
	return a
}
