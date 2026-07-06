package gitcmd

import (
	"reflect"
	"testing"
)

func TestParseStatusPorcelain(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   []StatusEntry
	}{
		{
			name:   "empty means clean",
			output: "",
			want:   nil,
		},
		{
			name:   "untracked file",
			output: "?? notes/new.md\n",
			want:   []StatusEntry{{X: '?', Y: '?', Path: "notes/new.md"}},
		},
		{
			name:   "modified staged and unstaged",
			output: "M  staged.md\n M unstaged.md\nMM both.md\n",
			want: []StatusEntry{
				{X: 'M', Y: ' ', Path: "staged.md"},
				{X: ' ', Y: 'M', Path: "unstaged.md"},
				{X: 'M', Y: 'M', Path: "both.md"},
			},
		},
		{
			name:   "rename",
			output: "R  old.md -> new.md\n",
			want:   []StatusEntry{{X: 'R', Y: ' ', Path: "new.md", OrigPath: "old.md"}},
		},
		{
			name:   "unmerged conflict",
			output: "UU conflicted.md\n",
			want:   []StatusEntry{{X: 'U', Y: 'U', Path: "conflicted.md"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStatusPorcelain(c.output)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseStatusPorcelain(%q) = %#v, want %#v", c.output, got, c.want)
			}
		})
	}
}

func TestStatusEntryIsConflicted(t *testing.T) {
	cases := []struct {
		name  string
		entry StatusEntry
		want  bool
	}{
		{"unmerged both", StatusEntry{X: 'U', Y: 'U'}, true},
		{"unmerged staged only", StatusEntry{X: 'U', Y: ' '}, true},
		{"added both", StatusEntry{X: 'A', Y: 'A'}, true},
		{"deleted both", StatusEntry{X: 'D', Y: 'D'}, true},
		{"ordinary modified", StatusEntry{X: 'M', Y: ' '}, false},
		{"untracked", StatusEntry{X: '?', Y: '?'}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.entry.IsConflicted(); got != c.want {
				t.Errorf("%+v.IsConflicted() = %v, want %v", c.entry, got, c.want)
			}
		})
	}
}

func TestParseLeftRightCount(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantAhead  int
		wantBehind int
		wantErr    bool
	}{
		{"equal", "0\t0\n", 0, 0, false},
		{"ahead", "3\t0\n", 3, 0, false},
		{"behind", "0\t2\n", 0, 2, false},
		{"diverged", "1\t4\n", 1, 4, false},
		{"malformed", "not a number\n", 0, 0, true},
		{"missing field", "1\n", 0, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ahead, behind, err := parseLeftRightCount(c.output)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseLeftRightCount(%q) error = %v, wantErr %v", c.output, err, c.wantErr)
			}
			if err != nil {
				return
			}
			if ahead != c.wantAhead || behind != c.wantBehind {
				t.Errorf("parseLeftRightCount(%q) = (%d, %d), want (%d, %d)", c.output, ahead, behind, c.wantAhead, c.wantBehind)
			}
		})
	}
}
