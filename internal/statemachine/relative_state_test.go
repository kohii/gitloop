package statemachine

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name          string
		ahead, behind int
		want          RelativeState
	}{
		{"equal", 0, 0, Equal},
		{"ahead", 3, 0, Ahead},
		{"behind", 0, 2, Behind},
		{"diverged", 1, 1, Diverged},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.ahead, c.behind); got != c.want {
				t.Errorf("Classify(%d, %d) = %v, want %v", c.ahead, c.behind, got, c.want)
			}
		})
	}
}

func TestRelativeStateString(t *testing.T) {
	cases := []struct {
		state RelativeState
		want  string
	}{
		{Equal, "equal"},
		{Ahead, "ahead"},
		{Behind, "behind"},
		{Diverged, "diverged"},
		{RelativeState(99), "unknown"},
	}

	for _, c := range cases {
		if got := c.state.String(); got != c.want {
			t.Errorf("RelativeState(%d).String() = %q, want %q", c.state, got, c.want)
		}
	}
}
