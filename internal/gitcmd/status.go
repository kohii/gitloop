package gitcmd

import "strings"

// StatusEntry is one line of `git status --porcelain` output: the two
// status letters (staged/unstaged) and the path they apply to.
type StatusEntry struct {
	// X is the staged (index) status code, Y is the unstaged (worktree) one.
	// See `git help status` "Short Format" for the full code table.
	X, Y byte
	// Path is the current path. For renames/copies this is the new path.
	Path string
	// OrigPath is the previous path, set only for renames/copies.
	OrigPath string
}

// IsConflicted reports whether the entry represents an unmerged (conflicted)
// path, as opposed to an ordinary staged/unstaged change.
func (e StatusEntry) IsConflicted() bool {
	switch {
	case e.X == 'U' || e.Y == 'U':
		return true
	case e.X == 'A' && e.Y == 'A':
		return true
	case e.X == 'D' && e.Y == 'D':
		return true
	default:
		return false
	}
}

// StatusPorcelain runs `git status --porcelain` and returns the parsed
// entries.
func (r *Runner) StatusPorcelain() ([]StatusEntry, error) {
	res, err := r.run("status", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseStatusPorcelain(res.Stdout), nil
}

// parseStatusPorcelain parses `git status --porcelain` (v1, unquoted paths)
// output into structured entries. Each line is "XY PATH" or, for
// renames/copies, "XY ORIG -> PATH".
func parseStatusPorcelain(output string) []StatusEntry {
	var entries []StatusEntry
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		rest := line[3:]

		entry := StatusEntry{X: x, Y: y, Path: rest}
		if idx := strings.Index(rest, " -> "); idx >= 0 {
			entry.OrigPath = rest[:idx]
			entry.Path = rest[idx+len(" -> "):]
		}
		entries = append(entries, entry)
	}
	return entries
}
