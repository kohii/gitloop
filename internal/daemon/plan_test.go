package daemon

import (
	"testing"

	"github.com/kohii/gitloop/internal/statemachine"
)

func upstreamAt(state statemachine.RelativeState) *statemachine.RelativeState {
	return &state
}

func TestPlanCycle(t *testing.T) {
	cases := []struct {
		name        string
		autoCommits bool
		obs         cycleObservation
		want        cycleIntent
		wantBlocked string
	}{
		// A repository with no remote, and one whose fetch just failed, look
		// the same from here: no upstream to classify against, so the only
		// work left is preserving local edits.
		{
			name:        "no upstream data and a clean tree has nothing to do",
			autoCommits: true,
			obs:         cycleObservation{},
			want:        intentNothingToDo,
		},
		{
			name:        "no upstream data still commits a dirty tree",
			autoCommits: true,
			obs:         cycleObservation{Dirty: true},
			want:        intentMutate,
		},

		{
			name:        "auto-commit: clean and in sync is the common no-op",
			autoCommits: true,
			obs:         cycleObservation{Upstream: upstreamAt(statemachine.Equal)},
			want:        intentNothingToDo,
		},
		{
			name:        "auto-commit: dirty and in sync commits",
			autoCommits: true,
			obs:         cycleObservation{Dirty: true, Upstream: upstreamAt(statemachine.Equal)},
			want:        intentMutate,
		},
		{
			name:        "auto-commit: clean and ahead only needs a push",
			autoCommits: true,
			obs:         cycleObservation{Upstream: upstreamAt(statemachine.Ahead)},
			want:        intentPush,
		},
		{
			name:        "auto-commit: dirty and ahead commits first",
			autoCommits: true,
			obs:         cycleObservation{Dirty: true, Upstream: upstreamAt(statemachine.Ahead)},
			want:        intentMutate,
		},
		{
			name:        "auto-commit: behind writes the incoming commits into the tree",
			autoCommits: true,
			obs:         cycleObservation{Upstream: upstreamAt(statemachine.Behind)},
			want:        intentMutate,
		},
		{
			name:        "auto-commit: diverged merges",
			autoCommits: true,
			obs:         cycleObservation{Upstream: upstreamAt(statemachine.Diverged)},
			want:        intentMutate,
		},

		// A committed-sync repository never stages anything, so a dirty tree
		// on its own is not work — it is the user's business.
		{
			name: "committed-sync: dirty and in sync is still a no-op",
			obs:  cycleObservation{Dirty: true, Upstream: upstreamAt(statemachine.Equal)},
			want: intentNothingToDo,
		},
		{
			name: "committed-sync: dirty and ahead pushes without touching the tree",
			obs:  cycleObservation{Dirty: true, Upstream: upstreamAt(statemachine.Ahead)},
			want: intentPush,
		},
		{
			name: "committed-sync: behind attempts the fast-forward",
			obs:  cycleObservation{Upstream: upstreamAt(statemachine.Behind)},
			want: intentMutate,
		},
		{
			name: "committed-sync: dirty and behind still attempts the fast-forward",
			obs:  cycleObservation{Dirty: true, Upstream: upstreamAt(statemachine.Behind)},
			want: intentMutate,
		},
		{
			name:        "committed-sync: diverged is blocked, not merged",
			obs:         cycleObservation{Upstream: upstreamAt(statemachine.Diverged)},
			want:        intentBlocked,
			wantBlocked: BlockedDiverged,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planCycle(tc.autoCommits, tc.obs)
			if got.Intent != tc.want {
				t.Errorf("planCycle() intent = %s, want %s", got.Intent, tc.want)
			}
			if got.BlockedReason != tc.wantBlocked {
				t.Errorf("planCycle() blocked reason = %q, want %q", got.BlockedReason, tc.wantBlocked)
			}
		})
	}
}
