package commitmsg

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind categorizes a single file change for the purpose of summarizing it in
// a commit message.
type Kind int

const (
	// Added means the file is new (untracked or newly staged).
	Added Kind = iota
	// Updated means an already-tracked file was modified.
	Updated
	// Deleted means a tracked file was removed.
	Deleted
	// Renamed means a tracked file was moved or renamed.
	Renamed
)

func (k Kind) label() string {
	switch k {
	case Added:
		return "added"
	case Updated:
		return "updated"
	case Deleted:
		return "deleted"
	case Renamed:
		return "renamed"
	default:
		return "changed"
	}
}

// Change is one file change to summarize in a commit message.
type Change struct {
	Kind Kind
	Path string
}

// maxListedPerKind is how many paths are named individually before a
// category collapses into a "N files (a, b, c, ...)" summary.
const maxListedPerKind = 3

// Build renders the auto-commit message for a set of working-tree changes.
// Format: "[<host>] <date> <time> — <summary>", e.g.
//
//	[macbook-air] 2026-07-07 15:30 — updated: notes/todo.md, added: notes/2026-07-07.md
func Build(host string, at time.Time, changes []Change) string {
	header := fmt.Sprintf("[%s] %s", host, at.Format("2006-01-02 15:04"))
	summary := summarize(changes)
	if summary == "" {
		return header
	}
	return header + " — " + summary
}

func summarize(changes []Change) string {
	byKind := make(map[Kind][]string, 4)
	for _, c := range changes {
		byKind[c.Kind] = append(byKind[c.Kind], c.Path)
	}

	var parts []string
	for _, k := range []Kind{Added, Updated, Deleted, Renamed} {
		paths := byKind[k]
		if len(paths) == 0 {
			continue
		}
		sort.Strings(paths)
		parts = append(parts, formatKind(k, paths))
	}
	return strings.Join(parts, ", ")
}

func formatKind(k Kind, paths []string) string {
	label := k.label()
	if len(paths) <= maxListedPerKind {
		return fmt.Sprintf("%s: %s", label, strings.Join(paths, ", "))
	}
	sample := strings.Join(paths[:maxListedPerKind], ", ")
	return fmt.Sprintf("%s %d files (%s, ...)", label, len(paths), sample)
}
