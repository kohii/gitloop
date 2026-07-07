# gitloop

gitloop is a daemon that watches one or more git repositories' working trees
and keeps them synced with their remotes by automatically committing,
pushing, and merging. It works with any git repository — as the sync
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
  save_lock_path: ""   # "" disables save-lock coordination for every repository below
```

`~` in `path` is expanded to the user's home directory.

`save_lock_path` defaults to `<path>/.notesapp/state/save.lock` per
repository — see "Coordinating with another writer" below — and can be
overridden or set to `""` (disabling it) per repository or for all
repositories via `defaults`.

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
PATH                PHASE  LAST_COMMIT                LAST_PUSH                  LAST_ERROR
/Users/you/notes    idle   2026-07-07T07:07:47+09:00  2026-07-07T07:07:47+09:00  -
/Users/you/journal  -      -                          -                          not yet synced
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
   | >0    | >0     | merge, then push    |

See `docs/design.md` for the full state table and the conflict-resolution
flow diagram.

gitloop never runs `git reset --hard` or `git push --force`: every history
change it makes is an ordinary commit (an auto-commit, a merge commit, or a
merge commit carrying conflict backups), so nothing is ever discarded except
into `git reflog`'s normal safety net.

## Conflict resolution

If a merge stops on a real conflict, `on_conflict` decides what happens:

- **`claude`** (default): runs `claude -p` on each conflicted file to
  resolve the markers, then checks the file for leftover `<<<<<<<` /
  `=======` / `>>>>>>>` markers before accepting it. Requires **both**
  `ANTHROPIC_API_KEY` to be set and `claude` to be on `PATH`; if either is
  missing, or claude fails to produce a clean file, gitloop falls back to
  `backup` automatically.
- **`backup`**: saves both sides of each conflicted file next to the
  original, e.g. `notes/todo.conflict.macbook-air.20260707153000.ours.md`
  and `...theirs.md`, then accepts the upstream (`theirs`) version of each
  conflicted file and completes the merge commit. Nothing is lost — the
  discarded local content lives on in the `.ours.` file (and the commit
  that introduced it stays in `git log`/`git reflog`) — but reconciling it
  back into the real file is left to you.

Either way, a conflict is logged at `warn`/`error` level, and the
repository's status is reported as `phase: conflict` (see below) until the
next successful cycle.

## Status file and coordinating with another writer

gitloop writes `~/Library/Application Support/gitloop/status.json` after
every sync cycle (and on a 5s heartbeat independent of any cycle), so
another process — e.g. a notes-app server sharing the same working tree —
can watch gitloop's state without talking to it directly:

```json
{
  "pid": 4242,
  "last_heartbeat_at": "2026-07-07T15:30:05+09:00",
  "repos": {
    "/Users/you/notes": {
      "path": "/Users/you/notes",
      "phase": "idle",
      "last_commit": "2026-07-07T15:30:00+09:00",
      "last_push": "2026-07-07T15:30:05+09:00",
      "last_successful_sync_at": "2026-07-07T15:30:05+09:00",
      "last_error": "",
      "updated_at": "2026-07-07T15:30:05+09:00"
    }
  }
}
```

- **`pid`** / **`last_heartbeat_at`** let a consumer tell a crashed gitloop
  process apart from one that's merely idle: if `last_heartbeat_at` is
  stale (older than a few heartbeat intervals) or `pid` no longer exists,
  gitloop isn't running.
- **`repos[path].phase`** is `"idle"`, `"syncing"` (a cycle is running,
  fetch through push), or `"conflict"` (conflict backup files are sitting
  in the working tree, or an external rebase/merge is in progress). A
  writer sharing the repository should treat `syncing` and `conflict` as
  "don't touch the working tree right now".
- **`repos[path].last_successful_sync_at`** only advances when a full cycle
  completes without error, so a long-stale value — even while `last_error`
  looks clean — is a sign that syncing has quietly stopped working (e.g.
  expired push credentials).

### Coordinating with another writer

If some other process also writes directly to a watched repository (not
through gitloop), both sides need to avoid touching the working tree at the
same time. gitloop's half of that: before each cycle, it tries a
non-blocking `flock` on `save_lock_path` (default
`<repo path>/.notesapp/state/save.lock`); if that fails because the other
process is holding it, gitloop retries a few times (waiting `settle`
between attempts) and then skips the cycle for the next trigger to pick up.
The other process is expected to hold the same lock (also non-blocking) for
the duration of its own writes, and to treat `phase: syncing` / `conflict`
in the status file as "gitloop is mid-write, don't start a write of your
own". Set `save_lock_path: ""` to disable this for a repository with no
such external writer.

## Logs

`gitloop run` logs structured (`log/slog`) output to stderr; every
repository-scoped line carries a `repo=<basename>` attribute. Under
`gitloop install`, stdout/stderr are redirected to
`~/Library/Logs/gitloop.log`.

## License

MIT. See [LICENSE](./LICENSE).

## Related

gitloop grew out of the sync design for a personal note-taking app; the
broader vision for that project lives in that app's own repository and isn't
published here.
