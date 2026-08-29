package daemon

import "github.com/kohii/gitloop/internal/statemachine"

// cycleObservation is one read-only look at a repository, taken after the
// fetch and before the cycle decides whether it needs the save lock.
type cycleObservation struct {
	// Dirty reports whether the working tree or index has any pending change.
	Dirty bool
	// Upstream is where the local branch stands against the freshly fetched
	// upstream, or nil when this cycle has no usable upstream data: a
	// repository with no remote to fetch from, or one whose fetch just failed
	// and whose remote-tracking ref is therefore stale.
	Upstream *statemachine.RelativeState
}

// cycleIntent is what the rest of a sync cycle has to do, given what
// cycleObservation saw.
type cycleIntent int

const (
	// intentNothingToDo means the repository is already in the state the
	// cycle would put it in.
	intentNothingToDo cycleIntent = iota
	// intentPush means commits that already exist locally need to reach the
	// remote, and nothing else. Push writes to the remote, not to the
	// checkout, so it runs without the save lock.
	intentPush
	// intentBlocked means the cycle cannot proceed and must report why.
	intentBlocked
	// intentMutate means the cycle is going to write to the working tree —
	// commit, fast-forward, or merge — so it must hold the save lock.
	intentMutate
)

func (i cycleIntent) String() string {
	switch i {
	case intentNothingToDo:
		return "nothing-to-do"
	case intentPush:
		return "push"
	case intentBlocked:
		return "blocked"
	case intentMutate:
		return "mutate"
	default:
		return "unknown"
	}
}

// cyclePlan is the decision planCycle reached.
type cyclePlan struct {
	Intent cycleIntent
	// BlockedReason is set only for intentBlocked.
	BlockedReason string
}

// planCycle decides what a sync cycle must do, from the workflow's contract
// and one observation of the repository.
//
// The distinction it draws is which cycles write to the working tree, because
// those are the ones that have to hold the save lock — and holding it shuts an
// external writer out of the checkout for as long as it lasts. Most cycles are
// the timer finding a clean tree in sync with its upstream, and those must
// cost that writer nothing.
//
// The observation is a snapshot, and what the plan does with it differs by
// intent. intentMutate is only a decision to acquire the lock: the phases that
// run under it re-read the repository, because acquiring the lock can take
// several seconds and the world moves during them. For every other intent the
// snapshot is the whole answer — the cycle acts on it and stops. A change
// arriving just after an observation raises a file watcher event or waits for
// the next tick, so it belongs to the next cycle.
func planCycle(autoCommits bool, obs cycleObservation) cyclePlan {
	// Without fresh upstream data there is nothing to classify against, so
	// the only work still worth doing is preserving local edits as a commit.
	// A committed-sync repository never gets here: it has no auto-commit to
	// fall back on, and its caller reports the failed fetch instead.
	if obs.Upstream == nil {
		return mutateIf(autoCommits && obs.Dirty)
	}

	switch *obs.Upstream {
	case statemachine.Equal:
		return mutateIf(autoCommits && obs.Dirty)

	case statemachine.Ahead:
		// A dirty tree still has to be committed first under auto-commit; a
		// committed-sync repository leaves it alone and pushes what exists.
		if autoCommits && obs.Dirty {
			return cyclePlan{Intent: intentMutate}
		}
		return cyclePlan{Intent: intentPush}

	case statemachine.Behind:
		// Checking out the incoming commits writes to the working tree, even
		// when it is a plain fast-forward of a clean one.
		return cyclePlan{Intent: intentMutate}

	case statemachine.Diverged:
		if autoCommits {
			return cyclePlan{Intent: intentMutate}
		}
		return cyclePlan{Intent: intentBlocked, BlockedReason: BlockedDiverged}
	}

	return cyclePlan{Intent: intentMutate}
}

func mutateIf(mutates bool) cyclePlan {
	if mutates {
		return cyclePlan{Intent: intentMutate}
	}
	return cyclePlan{Intent: intentNothingToDo}
}
