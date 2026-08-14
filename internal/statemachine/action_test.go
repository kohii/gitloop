package statemachine

import "testing"

func TestActionFor(t *testing.T) {
	cases := []struct {
		state RelativeState
		want  Action
	}{
		{Equal, NoOp},
		{Ahead, Push},
		{Behind, FastForwardMerge},
		{Diverged, MergeThenPush},
	}

	for _, c := range cases {
		t.Run(c.state.String(), func(t *testing.T) {
			if got := ActionFor(c.state); got != c.want {
				t.Errorf("ActionFor(%v) = %v, want %v", c.state, got, c.want)
			}
		})
	}
}

func TestCommittedSyncActionFor(t *testing.T) {
	cases := []struct {
		state RelativeState
		want  Action
	}{
		{Equal, NoOp},
		{Ahead, Push},
		{Behind, FastForwardMerge},
		// A merge commit would be one gitloop authored itself, which the
		// workflow's contract forbids; replaying the local commits does not.
		{Diverged, RebaseThenPush},
	}

	for _, c := range cases {
		t.Run(c.state.String(), func(t *testing.T) {
			if got := CommittedSyncActionFor(c.state); got != c.want {
				t.Errorf("CommittedSyncActionFor(%v) = %v, want %v", c.state, got, c.want)
			}
		})
	}
}

func TestActionString(t *testing.T) {
	cases := []struct {
		action Action
		want   string
	}{
		{NoOp, "noop"},
		{Push, "push"},
		{FastForwardMerge, "fast-forward-merge"},
		{MergeThenPush, "merge-then-push"},
		{RebaseThenPush, "rebase-then-push"},
		{Action(99), "unknown"},
	}

	for _, c := range cases {
		if got := c.action.String(); got != c.want {
			t.Errorf("Action(%d).String() = %q, want %q", c.action, got, c.want)
		}
	}
}
