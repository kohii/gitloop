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
watching `~/notes` for changes. If it has a Git remote, gitloop syncs it using
`origin`; if it has no remotes, gitloop automatically uses commit-only mode.

A fuller example, overriding some settings per repository and some via the
shared `defaults` block:

```yaml
repositories:
  - path: ~/notes
  - path: ~/dev/journal
    settle: 5s
    on_conflict: claude
    save_lock_path: ~/notes/.myapp/state/save.lock
  - path: ~/scratch
    mode: commit-only  # no remote: auto-commit and nothing else
defaults:
  settle: 3s           # debounce: commit `settle` after the last file change
  max_wait: 60s        # ...but never wait longer than this while changes keep arriving
  fetch_interval: 30s  # also fetch on a timer, to notice remote-only changes
  mode: sync           # optional: "sync" or "commit-only"
  remote: origin
  branch: ""           # empty = whatever is currently checked out
  on_conflict: backup  # "backup" (default) or "claude" (opt-in, falls back to backup)
  save_lock_path: ""   # "" (default) disables save-lock coordination
```

For repositories where commits must always be made by a person, use an
explicit workflow. `committed-sync` never stages or creates a commit:

```yaml
repositories:
  - path: ~/dotfiles
    workflow:
      type: committed-sync
      remote: origin
      branch: main
      interval: 1m
```

The other workflow types correspond to the legacy `mode` values:
`auto-commit-sync` is used when a remote-backed sync is selected and
`auto-commit-only` is the local auto-commit behavior. `mode: committed-sync`
is also accepted as a legacy alias. A nested `workflow` cannot be combined
with `mode`.
Workflow-specific fields that do not apply to the selected type are rejected
at startup.

`~` in `path` is expanded to the user's home directory.

When `mode` is omitted, gitloop uses `sync` for repositories with at least one
configured Git remote and automatically uses `commit-only` when no remotes are
configured. An explicit `mode: sync` always remains sync mode, so a missing or
misspelled `remote` is reported as an error instead of being silently ignored.

`save_lock_path` is empty (disabled) by default. Set it explicitly — per
repository or via the `defaults` block — when gitloop shares a working
tree with another writer that agrees to hold the same lock. See
"Coordinating with another writer" below.

## Usage

```sh
gitloop run [--config <path>]        # start the daemon in the foreground
gitloop install [--config <path>]    # register + start the launchd agent
gitloop reload                       # validate config + restart the launchd agent
gitloop uninstall                    # stop + remove the launchd agent
gitloop status [--config <path>]     # show each repository's last sync state
gitloop lock hold <path>             # hold a save-lock on <path> until stdin closes
gitloop version
```

`gitloop lock hold` is a helper for external writers coordinating with
gitloop via `save_lock_path` (see "Coordinating with another writer" below).
A caller spawns it as a child process, waits for the line `"acquired\n"` on
stdout, does its own working-tree writes while the child holds the flock,
and closes the child's stdin (or exits, killing it) to release. Because the
flock is bound to the file descriptor, an unclean exit still releases the
lock — no cleanup handler needed on the caller side.

Exit codes let the caller tell contention apart from real failures:

| code | meaning |
|-----:|---------|
| 0    | acquired and released cleanly |
| 1    | I/O error (couldn't open path, unexpected flock failure) |
| 2    | usage error (missing/relative path, unknown flag) |
| 3    | lock already held by another process — caller can retry |

`gitloop install` writes `~/Library/LaunchAgents/dev.kohii.gitloop.plist`
(pointing at the `gitloop` binary's absolute path and the given `--config`)
and registers it with `launchctl bootstrap`. The agent restarts gitloop if
it crashes (`KeepAlive`) and starts it at login (`RunAtLoad`). One agent
runs regardless of how many repositories are configured.

After editing the configuration, run `gitloop reload`. It validates the config
path recorded in the installed plist and restarts the existing agent without
rewriting the plist. Use `gitloop install --config <path>` when changing the
config path or executable registration.

`gitloop status` reads the daemon's status file (see below) and prints a
table:

```
PATH                PHASE  LAST_COMMIT                LAST_PUSH                  LAST_ERROR      BLOCKED             LAST_AI_RESOLVE
/Users/you/notes    idle   2026-07-07T07:07:47+09:00  2026-07-07T07:07:47+09:00  -               -                    -
/Users/you/journal  -      -                          -                          not yet synced  -                    -
```

The `BLOCKED` column reports why a committed-sync repository is waiting,
such as `dirty-working-tree` or `diverged-history`.

## Sync behavior

On every trigger (a debounced file change, or the periodic fetch), gitloop
runs one cycle per repository:

1. **Guard** — skip the cycle if a rebase or merge is already in progress
   (detected by checking `.git/rebase-merge`, `.git/rebase-apply`,
   `.git/MERGE_HEAD`), so gitloop never interferes with something a human
   (or another tool) is in the middle of.
2. **Fetch** — retrieve the configured remote so the local branch can be
   classified against current upstream data.
3. **Auto-commit** — if the working tree is dirty, `git add -A` and commit
   with a generated message: `[<hostname>] <date> <time> — <change summary>`,
   e.g. `[macbook-air] 2026-07-07 15:30 — updated: notes/todo.md, added: notes/2026-07-07.md`.
4. **Classify and integrate**, then push when required:

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

### Commit-only repositories

Not every repository has a remote. When `mode` is omitted, gitloop detects this
and automatically uses `commit-only`, so the repository gets its edits
auto-committed without repeatedly attempting a fetch that cannot succeed.
The status remains healthy and `last_successful_sync_at` continues to mean
that the configured behavior is working.

`mode: commit-only` is the supported way to run gitloop purely as an
auto-commit daemon:

```yaml
repositories:
  - path: ~/journal
    mode: commit-only
```

The cycle then stops after the auto-commit phase: no fetch, no merge, no push, and
`remote` / `branch` are unused. `gitloop status` reports `LAST_PUSH` as
`n/a` for such a repository, so it can't be mistaken for a sync that has
stalled. `fetch_interval` still applies: with nothing to fetch it becomes a
periodic re-check that catches working-tree changes the file watcher missed
(dropped events, or edits made while the daemon was down).

An explicit `mode: sync` remains a loud choice: a typo'd `remote:` name, or a
repository with no remotes at all, stays a fetch failure instead of silently
downgrading to local-only commits. Setting `mode: commit-only` in the
`defaults` block therefore stops every repository that does not override it
from syncing.

### Committed-sync repositories

`workflow.type: committed-sync` is for repositories such as a shared dotfiles
checkout where people make commits explicitly, while gitloop transports those
commits between machines. It never runs `git add`, `git commit`, `git stash`,
`git reset`, or an automatic merge.

Each cycle fetches the remote and classifies the checked-out branch:

| working tree | state | action |
|--------------|-------|--------|
| clean or dirty | equal | nothing |
| clean or dirty | ahead | push existing commits |
| clean, or dirty outside the incoming commits | behind | fast-forward |
| dirty in a file the incoming commits rewrite | behind | defer and report `dirty-working-tree` |
| clean or dirty | diverged | defer and report `diverged-history` |

Fetching and pushing existing commits are safe while files are being edited,
and so is a fast-forward that rewrites none of them. gitloop just runs
`git merge --ff-only` and lets Git refuse — leaving the checkout untouched —
when the update would overwrite modified or untracked content, so editing one
file doesn't hold the whole repository behind upstream. Divergent histories
are left for a human to merge or rebase.

One thing this does not protect: a file you keep locally but `.gitignore` is
overwritten without warning if the remote starts tracking that path, because
Git treats ignored files as expendable. That is true of any `git merge`, not
just gitloop's.

The periodic `interval` is important because a manual commit changes `.git`,
which does not produce a watched working-tree event. If another process writes
the checkout concurrently, configure `save_lock_path` and have that process
hold the same advisory lock; without it, Git's own overwrite checks are the
final guard but there is no coordination handshake.

## Conflict resolution

If a merge stops on a real conflict, `on_conflict` decides what happens:

- **`backup`** (default): saves both sides of each conflicted file next to
  the original, e.g. `notes/todo.conflict.macbook-air.20260707153000.ours.md`
  and `...theirs.md`, then accepts the upstream (`theirs`) version of each
  conflicted file and completes the merge commit. Nothing is lost — the
  discarded local content lives on in the `.ours.` file (and the commit
  that introduced it stays in `git log`/`git reflog`) — but reconciling it
  back into the real file is left to you. Chosen as default because it
  never fails silently: no external dependencies, no API keys to expire.
- **`claude`** (opt-in): runs `claude -p` on each conflicted file to
  resolve the markers, then checks the file for leftover `<<<<<<<` /
  `=======` / `>>>>>>>` markers before accepting it. Requires **both**
  `ANTHROPIC_API_KEY` to be set and `claude` to be on `PATH`; if either is
  missing, or claude fails to produce a clean file, gitloop falls back to
  `backup` automatically. Under `gitloop install`, `launchd` runs the
  daemon without the user's shell env — so `ANTHROPIC_API_KEY` needs to
  live somewhere the plist inherits (e.g. `launchctl setenv
  ANTHROPIC_API_KEY <value>` in `~/.zprofile`, or the plist's
  `EnvironmentVariables` block). Otherwise the AI path silently falls
  back to `backup` on every conflict. A successful AI resolution's merge commit message
  is prefixed with `[ai-resolved]` (the prefix names the outcome, not the
  model, so it stays stable across model changes), e.g.
  `[ai-resolved] [macbook-air] 2026-07-07 15:30 — merged upstream (AI-resolved: notes/todo.md)` —
  so these commits stay identifiable in `git log` later if their content
  needs a second look. Because AI resolution can fail silently (expired
  key, network issue, model regression), `last_ai_resolve_at` /
  `last_ai_resolve_error` in the status file are the recommended way to
  watch this path's health when opting in.

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
      "last_ai_resolve_at": "2026-07-07T15:29:50+09:00",
      "last_ai_resolve_error": "",
      "updated_at": "2026-07-07T15:30:05+09:00"
    }
  }
}
```

- **`pid`** / **`last_heartbeat_at`** let a consumer tell a crashed gitloop
  process apart from one that's merely idle: if `last_heartbeat_at` is
  stale (older than a few heartbeat intervals) or `pid` no longer exists,
  gitloop isn't running.
- **`repos[path].phase`** is `"idle"`, `"syncing"` (the repository is in its
  local write/integration phase), or `"conflict"` (conflict backup files are sitting
  in the working tree, or an external rebase/merge is in progress). This
  is an observation, not a coordination protocol: another writer sharing
  the working tree should coordinate via `save_lock_path`, not by polling
  `phase`.
- **`repos[path].last_successful_sync_at`** only advances when a full cycle
  completes without error, so a long-stale value — even while `last_error`
  looks clean — is a sign that syncing has quietly stopped working (e.g.
  expired push credentials).
- **`repos[path].last_ai_resolve_at`** / **`last_ai_resolve_error`** cover
  the `on_conflict: claude` path specifically, which `last_error` doesn't:
  `last_error` only reflects the most recent whole cycle, so an AI
  resolution that has been silently failing (and falling back to `backup`)
  for a while can otherwise hide behind unrelated successful cycles.
  `last_ai_resolve_at` is stamped on the AI path's last success;
  `last_ai_resolve_error` holds the reason for its most recent failure and
  is cleared back to `""` the next time it succeeds.

### Coordinating with another writer

If some other process also writes directly to a watched repository (not
through gitloop), both sides need to avoid touching the working tree at
the same time. gitloop's half of that: before each cycle, it tries a
non-blocking `flock` on `save_lock_path`; if that fails because the other
process is holding it, gitloop retries a few times (waiting `settle`
between attempts) and then skips the cycle for the next trigger to pick
up. The other process is expected to hold the same lock (also
non-blocking) for the duration of its own writes.

`save_lock_path` is empty (disabled) by default. Configure it — per
repository or via the `defaults` block — with a path both writers agree
to hold, e.g. `~/notes/.myapp/state/save.lock`. The lock file itself is
created on demand and can live anywhere both writers can reach; putting
it inside the repository (in an app-namespaced hidden directory) keeps
its lifetime tied to the working tree.

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
