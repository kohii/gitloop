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
		{Diverged, RebaseThenPush},
	}

	for _, c := range cases {
		t.Run(c.state.String(), func(t *testing.T) {
			if got := ActionFor(c.state); got != c.want {
				t.Errorf("ActionFor(%v) = %v, want %v", c.state, got, c.want)
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
		{RebaseThenPush, "rebase-then-push"},
		{Action(99), "unknown"},
	}

	for _, c := range cases {
		if got := c.action.String(); got != c.want {
			t.Errorf("Action(%d).String() = %q, want %q", c.action, got, c.want)
		}
	}
}
