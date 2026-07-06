# gitloop

gitloop is a daemon that watches one or more git repositories' working trees
and keeps them synced with their remotes by automatically committing,
pushing, and rebasing. It works with any git repository — as the sync
backend for a note-taking app, or standalone for unrelated use cases such as
syncing an Obsidian vault to GitHub.

One gitloop process watches every repository listed in its config, each in
its own goroutine, so a problem in one repository never affects another.

**Status:** macOS only. Requires the system `git` CLI on `PATH`.

## Install

```sh
go install github.com/kohii/gitloop/cmd/gitloop@latest
```

## Configuration

gitloop reads YAML from `~/.config/gitloop/config.yaml` by default (override
with `--config`). The only required field per repository is `path`:

```yaml
repositories:
  - path: ~/notes
```

Every other setting has a default (see below), so this is enough to start
watching `~/notes` for changes and keep it synced with its `origin` remote.

A fuller example, overriding some settings per repository and some via the
shared `defaults` block:

```yaml
repositories:
  - path: ~/notes
  - path: ~/dev/journal
    settle: 5s
    on_conflict: backup
defaults:
  settle: 3s          # debounce: commit `settle` after the last file change
  max_wait: 60s        # ...but never wait longer than this while changes keep arriving
  fetch_interval: 5m   # also fetch on a timer, to notice remote-only changes
  remote: origin
  branch: ""           # empty = whatever is currently checked out
  on_conflict: claude  # "claude" (falls back to "backup") or "backup"
```

`~` in `path` is expanded to the user's home directory.

## Usage

```sh
gitloop run [--config <path>]        # start the daemon in the foreground
gitloop install [--config <path>]    # register + start the launchd agent
gitloop uninstall                    # stop + remove the launchd agent
gitloop status [--config <path>]     # show each repository's last sync state
gitloop version
```

`gitloop install` writes `~/Library/LaunchAgents/dev.kohii.gitloop.plist`
(pointing at the `gitloop` binary's absolute path and the given `--config`)
and registers it with `launchctl bootstrap`. The agent restarts gitloop if
it crashes (`KeepAlive`) and starts it at login (`RunAtLoad`). One agent
runs regardless of how many repositories are configured.

`gitloop status` reads the daemon's status file (see below) and prints a
table:

```
PATH                LAST_COMMIT              LAST_PUSH                LAST_ERROR
/Users/you/notes    2026-07-07T07:07:47+09:00  2026-07-07T07:07:47+09:00  -
/Users/you/journal  -                          -                          not yet synced
```

## Sync behavior

On every trigger (a debounced file change, or the periodic fetch), gitloop
runs one cycle per repository:

1. **Guard** — skip the cycle if a rebase or merge is already in progress
   (detected by checking `.git/rebase-merge`, `.git/rebase-apply`,
   `.git/MERGE_HEAD`), so gitloop never interferes with something a human
   (or another tool) is in the middle of.
2. **Auto-commit** — if the working tree is dirty, `git add -A` and commit
   with a generated message: `[<hostname>] <date> <time> — <change summary>`,
   e.g. `[macbook-air] 2026-07-07 15:30 — updated: notes/todo.md, added: notes/2026-07-07.md`.
3. **Fetch**, then classify the local branch against its upstream:

   | ahead | behind | action              |
   |------:|-------:|---------------------|
   | 0     | 0      | nothing to do       |
   | >0    | 0      | push                |
   | 0     | >0     | fast-forward merge  |
   | >0    | >0     | rebase, then push   |

See `docs/design.md` for the full state table and the conflict-resolution
flow diagram.

## Conflict resolution

If a rebase stops on a real conflict, `on_conflict` decides what happens:

- **`claude`** (default): runs `claude -p` on each conflicted file to
  resolve the markers, then checks the file for leftover `<<<<<<<` /
  `=======` / `>>>>>>>` markers before accepting it. Requires **both**
  `ANTHROPIC_API_KEY` to be set and `claude` to be on `PATH`; if either is
  missing, or claude fails to produce a clean file, gitloop falls back to
  `backup` automatically.
- **`backup`**: saves both sides of each conflicted file next to the
  original, e.g. `notes/todo.conflict.macbook-air.20260707153000.ours.md`
  and `...theirs.md`, aborts the rebase, resets the branch to upstream, and
  commits the backup files. Nothing is lost — the discarded local commit's
  content lives on in the `.theirs.` file (and in `git reflog`) — but
  reconciling it back into the real file is left to you.

Either way, a conflict is logged at `warn`/`error` level; gitloop does not
retry the same conflict indefinitely.

## Logs

`gitloop run` logs structured (`log/slog`) output to stderr; every
repository-scoped line carries a `repo=<basename>` attribute. Under
`gitloop install`, stdout/stderr are redirected to
`~/Library/Logs/gitloop.log`. Status snapshots are written to
`~/Library/Caches/gitloop/status.json`.

## License

MIT. See [LICENSE](./LICENSE).

## Related

gitloop grew out of the sync design for a personal note-taking app; the
broader vision for that project lives in that app's own repository and isn't
published here.
