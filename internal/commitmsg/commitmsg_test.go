package commitmsg

import (
	"testing"
	"time"
)

func TestBuild(t *testing.T) {
	at := time.Date(2026, 7, 7, 15, 30, 0, 0, time.UTC)

	cases := []struct {
		name    string
		changes []Change
		want    string
	}{
		{
			name:    "no changes",
			changes: nil,
			want:    "[macbook-air] 2026-07-07 15:30",
		},
		{
			name: "single added and updated",
			changes: []Change{
				{Kind: Updated, Path: "notes/todo.md"},
				{Kind: Added, Path: "notes/2026-07-07.md"},
			},
			want: "[macbook-air] 2026-07-07 15:30 — added: notes/2026-07-07.md, updated: notes/todo.md",
		},
		{
			name: "many files in one kind collapse to a summary",
			changes: []Change{
				{Kind: Updated, Path: "a.md"},
				{Kind: Updated, Path: "b.md"},
				{Kind: Updated, Path: "c.md"},
				{Kind: Updated, Path: "d.md"},
				{Kind: Updated, Path: "e.md"},
			},
			want: "[macbook-air] 2026-07-07 15:30 — updated 5 files (a.md, b.md, c.md, ...)",
		},
		{
			name: "all kinds present",
			changes: []Change{
				{Kind: Added, Path: "new.md"},
				{Kind: Updated, Path: "old.md"},
				{Kind: Deleted, Path: "gone.md"},
				{Kind: Renamed, Path: "moved.md"},
			},
			want: "[macbook-air] 2026-07-07 15:30 — added: new.md, updated: old.md, deleted: gone.md, renamed: moved.md",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Build("macbook-air", at, c.changes); got != c.want {
				t.Errorf("Build(...) = %q, want %q", got, c.want)
			}
		})
	}
}
