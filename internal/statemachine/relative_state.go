package statemachine

// RelativeState classifies how a local branch stands against its upstream
// after a fetch, as derived from `git rev-list --left-right --count`.
type RelativeState int

const (
	// Equal means the local branch and upstream point at the same commit.
	Equal RelativeState = iota
	// Ahead means the local branch has commits upstream does not.
	Ahead
	// Behind means upstream has commits the local branch does not.
	Behind
	// Diverged means both sides have commits the other lacks.
	Diverged
)

// String returns the lower-case name of the state, e.g. "equal".
func (s RelativeState) String() string {
	switch s {
	case Equal:
		return "equal"
	case Ahead:
		return "ahead"
	case Behind:
		return "behind"
	case Diverged:
		return "diverged"
	default:
		return "unknown"
	}
}

// Classify maps the ahead/behind commit counts reported by
// `git rev-list --left-right --count <local>...<upstream>` to a RelativeState.
func Classify(ahead, behind int) RelativeState {
	switch {
	case ahead == 0 && behind == 0:
		return Equal
	case ahead > 0 && behind == 0:
		return Ahead
	case ahead == 0 && behind > 0:
		return Behind
	default:
		return Diverged
	}
}
