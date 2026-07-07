package statemachine

// Action is the sync operation to run for a given RelativeState.
type Action int

const (
	// NoOp means local and remote already match; nothing to do.
	NoOp Action = iota
	// Push means the local branch has commits the remote lacks; push them.
	Push
	// FastForwardMerge means the remote has commits the local branch lacks
	// and no local commits are ahead; fast-forward the local branch.
	FastForwardMerge
	// MergeThenPush means both sides have diverged; merge upstream into the
	// local branch (fast-forwarding if possible, otherwise a merge commit),
	// then push.
	MergeThenPush
)

// String returns the lower-case name of the action, e.g. "push".
func (a Action) String() string {
	switch a {
	case NoOp:
		return "noop"
	case Push:
		return "push"
	case FastForwardMerge:
		return "fast-forward-merge"
	case MergeThenPush:
		return "merge-then-push"
	default:
		return "unknown"
	}
}

// ActionFor maps a RelativeState to the sync action it calls for.
func ActionFor(s RelativeState) Action {
	switch s {
	case Ahead:
		return Push
	case Behind:
		return FastForwardMerge
	case Diverged:
		return MergeThenPush
	default: // Equal, or an unrecognized state
		return NoOp
	}
}
