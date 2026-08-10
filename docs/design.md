# Design notes

This documents the choices that aren't obvious from reading the code:
why the dependencies are what they are, the state table the sync loop is
built around, how a conflict is actually resolved end to end, and what the
generated launchd agent looks like.

## Dependencies

- **System `git` CLI via `os/exec`**, not `go-git` or `libgit2`. gitloop
  needs `merge`, `merge --abort`, and index stages during a conflict
  (`git show :2:<path>` / `:3:<path>`) — all exactly as the system git
  implements them. A Go git library would mean re-implementing or
  approximating that behavior; shelling out gets it for free and stays
  compatible with whatever git version and config (credential helpers,
  hooks) the user already has.
- **`github.com/fsnotify/fsnotify`** for file watching. It's the de facto
  standard cross-platform (kqueue/inotify/ReadDirectoryChangesW) wrapper;
  nothing gitloop needs justifies a custom syscall layer.
- **`gopkg.in/yaml.v3`** for config parsing. Config is a small, mostly-flat
  structure; a full config framework would be more machinery than the
  problem needs.
- **`golang.org/x/sys/unix`** for `flock` (`unix.Flock`), used by the save
  lock (see "Status file" below) to coordinate with another process writing
  to the same repository. The standard library has no portable advisory
  file locking primitive.

No other third-party dependencies. `internal/statemachine` and
`internal/commitmsg` are pure Go with no dependencies at all, by design —
see "Package boundaries" below.

## Package boundaries

- `internal/statemachine` — decides *what* to do (classify ahead/behind
  counts, map to an action, guard against an in-progress rebase/merge).
  Pure functions over inputs the caller already has; no `os/exec`, no I/O
  beyond a filesystem stat in `PreCheck`. This is what makes exhaustive
  table tests possible.
- `internal/gitcmd` — runs git and parses its output into structs. Knows
  nothing about *when* to call anything.
- `internal/commitmsg` — pure formatting (host + time + change summary ->
  string). No git or filesystem access; testable with plain data.
- `internal/config` — YAML -> validated `Config`, defaults applied, `~`
  expanded. No knowledge of git or the daemon loop.
- `internal/daemon` — the only package that wires the others together:
  fsnotify, debounce timers, per-repository goroutines, conflict recovery,
  status persistence. It depends on a `GitClient` interface (satisfied by
  `gitcmd.Runner`) rather than `gitcmd` directly, so tests drive the loop
  against an in-memory fake instead of a real checkout.

### Workflow contracts

The legacy `mode` values remain supported, but new configurations may use a
nested `workflow` tag. The three workflow contracts are:

- `auto-commit-sync` — the existing auto-commit, fetch, integrate, and push
  behavior.
- `auto-commit-only` — the existing local auto-commit behavior.
- `committed-sync` — transport commits that already exist, without ever
  staging or creating a commit.

`committed-sync` fetches on every cycle, pushes an ahead branch even when the
working tree is dirty, fast-forwards only a clean working tree, and refuses
diverged histories. It never creates a merge commit: a human must resolve a
divergence before gitloop can transport the resulting commits. The dirty check
is repeated immediately before a fast-forward while the save lock is held, so
a save arriving during classification cannot be overwritten.

## Sync state table

After `git fetch`, `git rev-list --left-right --count <branch>...<remote>/<branch>`
gives (ahead, behind), classified into 4 states with one action each:

| ahead | behind | state    | action              |
|------:|-------:|----------|---------------------|
| 0     | 0      | Equal    | no-op               |
| >0    | 0      | Ahead    | push                |
| 0     | >0     | Behind   | fast-forward merge  |
| >0    | >0     | Diverged | merge, then push    |

Before any of this runs, `statemachine.PreCheck` stats `.git/rebase-merge`,
`.git/rebase-apply`, and `.git/MERGE_HEAD` (following the `gitdir:`
indirection for worktrees). If any exist, the cycle is skipped entirely —
gitloop never touches a repository that a human (or another tool) is in the
middle of operating on. gitloop itself never leaves a rebase in progress
(it doesn't use `git rebase` at all), so a rebase marker found here always
means an external tool or a person started one by hand.

Reusing the same cycle (guard -> fetch -> commit-if-dirty -> classify -> act)
for all three triggers — settle timer, max-wait timer, and the periodic
fetch tick — is deliberate. The commit step is a no-op on a clean working
tree, so "fetch periodically to notice remote changes" doesn't need a
separate code path from "the user just saved a file."

**Commit-only repositories stop after the commit step.** `mode:
commit-only` (`config.ModeCommitOnly`) makes `runRepoLoop` run
guard -> commit-if-dirty and nothing else: no fetch, no classification
against the table above, no push. The daemon also selects this mode when
`mode` was omitted and `git remote` reports no configured remotes. A
repository with no remote would otherwise fail `git fetch` every cycle — the
auto-commit itself would still land, since fetch failure is deliberately
non-fatal (see "Save lock" below), but `last_error` would never clear and
`last_successful_sync_at` would never advance.

An explicit `mode: sync` remains a loud choice: a typo'd remote name or a
repository with no remotes stays a fetch failure instead of silently
downgrading to local-only commits. The periodic timer keeps running in
commit-only mode: with nothing to fetch it becomes the safety net for
working-tree changes the watcher missed.

**Committed-sync repositories stop before the commit step.** They still fetch
and classify the local branch, but only take these actions: no-op for Equal,
push for Ahead, fast-forward for clean Behind, and a reported blocked state
for dirty Behind or Diverged. This mode is intentionally stricter than a
generic `auto_commit: false` flag: it prevents a future integration policy
from silently creating merge commits under a workflow whose contract is that
commits are human-created.

## Conflict flow

`Diverged` maps to `merge, then push`. gitloop never uses `git rebase` or
`git reset --hard`: a vault-app contract this daemon must respect forbids
rewriting history internally, and `reset --hard` in particular used to
discard every local commit down to upstream on a conflict — including
commits to files that had nothing to do with the conflict. A merge only
ever touches the files it can't auto-resolve, so non-conflicting local
commits are always preserved as part of the merge commit.

`git merge --no-ff --no-edit <upstream>` either fast-forwards (never
actually reachable here, since Diverged means neither side is an ancestor
of the other; `--no-ff` just makes that explicit so git doesn't ask which
mode was intended) or creates a merge commit. If it stops on a conflict,
control passes to `resolveConflicts`, which — unlike a rebase, where each
replayed commit can conflict in turn — sees every conflicting file in one
shot and resolves the whole set in a single pass:

```
merge reports conflict
        │
        ▼
policy == claude AND claude CLI + ANTHROPIC_API_KEY available?
        │                                   │
       yes                                  no
        │                                   │
        ▼                                   ▼
run `claude -p` per conflicted file    (fall through to backup)
check markers gone, `git add`
        │
   all files clean?
    │        │
   yes       no ──► record last_ai_resolve_error
    │        │
    ▼        ▼
commit the merge               back up ours/theirs per file,
(phase stays "idle",           `git checkout --theirs` + `git add` each,
 last_ai_resolve_at stamped,   commit the merge
 message prefixed              (phase becomes "conflict")
 "[ai-resolved]")
    │                                   │
    ▼                                   ▼
   push  ◄────────────────────────────────┘
```

**Why accept theirs instead of ours:** upstream has already been pushed and
may be visible to other devices, so it's the version of record; the local
("ours") side is still-unshared and would otherwise be silently discarded
by taking upstream's content, so it's rescued into a backup file instead.
Nothing is lost either way: both sides' content is preserved in the backup
files, and the commit that introduced the local side stays reachable in
`git log`/`git reflog`. This also keeps the repository from re-diverging —
because the merge (with theirs accepted) actually completes and gets
pushed, the next cycle sees a clean Equal/Ahead state instead of hitting
the identical conflict again.

**Ours vs. theirs naming**: during a merge (unlike a rebase, where the
convention is inverted), "ours" (index stage 2) is the local branch and
"theirs" (stage 3) is the branch being merged in — i.e. upstream. The
backup filenames follow that same convention, since they come straight from
`git show :2:<path>` / `:3:<path>`.

**Claude availability check requires both** `ANTHROPIC_API_KEY` being set
and `claude` being on `PATH`. Either missing means the `claude` policy is
treated as unavailable for that cycle and falls back to `backup` — it never
partially attempts a claude resolution. Either path finishes with a plain
`git commit` (not `git merge --continue` — they're equivalent once
`MERGE_HEAD` exists and the index is fully staged, but `commit` lets
gitloop supply its own message).

**AI-resolved commits are marked, not just logged**: a successful claude
resolution's commit message gets a `[ai-resolved]` prefix (see
`commitmsg.BuildConflictResolution`), naming the outcome rather than the
model so the marker doesn't need to change if the resolver behind
`on_conflict: claude` ever does. This is the only way to tell an
AI-resolved merge apart from a human/backup one after the fact by reading
`git log` alone. `tryResolveWithClaude` also reports success/failure to the
status file (`last_ai_resolve_at` / `last_ai_resolve_error`) independently
of the cycle-level `last_error`, because a `claude` policy that has quietly
started failing every cycle (e.g. an expired `ANTHROPIC_API_KEY` under
launchd) still falls back to `backup` and completes the cycle "successfully"
— `last_error` alone would never surface that.

## launchd agent

`gitloop install` writes `~/Library/LaunchAgents/dev.kohii.gitloop.plist`
(see `cmd/gitloop/launchagent.go`, `buildPlist`) and registers it with
`launchctl bootstrap gui/<uid> <plist path>`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.kohii.gitloop</string>
	<key>ProgramArguments</key>
	<array>
		<string>/absolute/path/to/gitloop</string>
		<string>run</string>
		<string>--config</string>
		<string>/absolute/path/to/config.yaml</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/Users/<you>/Library/Logs/gitloop.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/<you>/Library/Logs/gitloop.log</string>
</dict>
</plist>
```

One agent, one process, regardless of how many repositories are configured
— multi-repo support lives entirely inside the daemon (one goroutine per
repository), not in the launchd layer. `KeepAlive` restarts the process if
it crashes; `gitloop uninstall` runs `launchctl bootout gui/<uid>/dev.kohii.gitloop`
and removes the plist. `gitloop reload` validates the config path stored in
the plist and uses `launchctl kickstart -k` to restart the existing agent
without rewriting its registration.

## Status file

The daemon writes `~/Library/Application Support/gitloop/status.json`
(atomically: write to `.tmp`, then rename) after every sync cycle, plus on
a fixed heartbeat independent of any cycle, so `gitloop status` — or any
other process, e.g. a notes-app server sharing the repository — can read
gitloop's state without talking to the running daemon over IPC. Multiple
repositories share one file, keyed by repository path, guarded by a mutex
inside the daemon process. Application Support is used rather than
`~/Library/Caches` because macOS may purge the latter at any time, and this
file needs to be durable enough for another process to rely on it for
crash/staleness detection.

**Top-level fields** (not per-repository):

- `pid` — the daemon's own process ID, written once at startup.
- `last_heartbeat_at` — stamped every 5 seconds by `Daemon.runHeartbeat`, a
  goroutine independent of every repository's watch/sync loop. A consumer
  combines this with `pid` to tell "gitloop crashed" apart from "gitloop is
  just idle or backed off": if `last_heartbeat_at` is stale or `pid` no
  longer exists, treat gitloop as not running regardless of what any
  individual repository's `phase` last said.

**Per-repository fields** (`repos[<path>]`):

- `phase` — `"idle"`, `"syncing"`, or `"conflict"`. `runRepoLoop` sets
  `syncing` immediately before the local write/integration phase and
  resolves it to `idle` or `conflict` when the cycle ends — `conflict` if
  the cycle just committed a backup-and-accept-theirs merge (see "Conflict
  flow"), or if `PreCheck` found a rebase/merge already in progress;
  `idle` otherwise, including on a plain error. A writer sharing the
  repository is expected to treat `syncing`/`conflict` as "don't touch the
  working tree right now".
- `last_successful_sync_at` — stamped only when a full cycle (fetch through
  push, or a no-op because nothing needed syncing) completes without error.
  Unlike `last_error`, which only reflects the most recent cycle, a
  long-stale value here is a sign that syncing has quietly stopped working
  (e.g. expired push credentials) even if nothing is visibly failing loudly.
- `last_commit` / `last_push` / `last_error` / `updated_at` — unchanged
  from before: timestamps of the last auto-commit and push, the most recent
  cycle's error (or a `"skipped: ..."` guard/lock reason), and when the
  entry was last written.
- `blocked_reason` — a concise reason a committed-sync cycle deliberately
  stopped, such as `dirty-working-tree`, `diverged-history`, or
  `operation-in-progress`. It is cleared by the next cycle that completes
  without a block.
- `last_ai_resolve_at` / `last_ai_resolve_error` — the `on_conflict: claude`
  path's own success/failure, stamped by `tryResolveWithClaude` independently
  of `last_error`. `last_ai_resolve_at` advances only when claude actually
  produces a clean, marker-free resolution; `last_ai_resolve_error` holds the
  reason for the most recent failed attempt and is cleared on the next
  success. `gitloop status` surfaces this as a `LAST_AI_RESOLVE` column
  (the error takes priority over the timestamp when both would apply).

## Save lock: coordinating with another writer

Some deployments have a second process writing directly to the same
working tree — for example, a notes-app server handling explicit saves
while gitloop handles background sync. Both sides need to avoid touching
the tree at the same time, so gitloop treats an advisory lock file as a
handshake:

- `Repository.SaveLockPath` (config key `save_lock_path`) is empty by
  default, which disables the whole mechanism; repositories with such an
  external writer set it explicitly to a path both sides agree to hold.
- Each cycle is split by `runRepoLoop` into three phases so the save lock
  is only held for the part that actually touches the working tree:
    1. **Pre-fetch (no lock)**: `PreCheck` + `git fetch`. Fetch is a network
       round-trip that only writes under `.git/`, so blocking an external
       writer on it would be pure latency for no safety win. A failed fetch
       does *not* abort the cycle: the daemon still acquires the lock and
       runs the commit phase, so local edits are captured even when the
       laptop is offline; only the integrate + push phases are skipped in
       that case, and the fetch error surfaces via `last_error`.
    2. **Commit + integrate (lock held)**: acquire the save lock via
       `acquireSaveLockWithRetry` (`internal/daemon/savelock.go`);
       auto-commit any dirty changes, classify against the freshly fetched
       upstream, and merge or fast-forward if the local branch is behind
       or diverged (running the conflict-resolution policy on a conflict).
       Every step here writes to the index or the working tree, so the
       lock is what keeps an external writer out. `lock.release()` runs
       via `defer` in addition to being called explicitly at the end of
       the phase, so a panic inside a merge or conflict-resolution step
       can't leak the flock and deadlock every subsequent cycle.
       In `committed-sync`, this phase only checks status and performs a
       clean-tree fast-forward; it never stages, commits, or merges.
    3. **Push (lock released)**: push if the state was Ahead or Diverged.
       Like fetch, this is network I/O that doesn't touch the working
       tree; holding the lock across a slow push would just delay the
       external writer.
- If the save lock is held by the other writer when the commit/integrate
  phase tries to acquire it, `acquireSaveLockWithRetry` retries a few
  times (waiting `Settle` between attempts) and then the cycle is skipped
  entirely, leaving `last_error` as `"skipped: save in-flight"` without
  touching `phase`. The next trigger (file change or fetch timer) tries
  again from scratch.
- `phase` is only advanced to `syncing` once the commit/integrate phase
  begins — during the pre-fetch phase `phase` stays at its previous
  resting value (usually `idle`). It is dropped back to `idle` the moment
  the commit/integrate phase releases the lock, *before* the push runs,
  so an external observer sees `syncing` for exactly the interval an
  external writer actually needs to stay out of the tree — nothing more.
- The lock is released as soon as the commit/integrate phase finishes; it
  does not stay held during the push phase or the settle/debounce window
  that precedes the next trigger. A push failure surfaces via `last_error`
  but leaves `phase` at `idle`: the working tree is clean and the next
  trigger will retry naturally.
- The other writer is expected to hold the same lock (also non-blocking,
  so it never gets stuck queuing behind gitloop) for the duration of its
  own writes. `gitloop lock hold <path>` (`cmd/gitloop/lock.go`) is a
  ready-made helper for that: the external writer spawns it as a child
  process, waits for the line `"acquired\n"` on stdout, does its writes,
  and closes the child's stdin (or exits) to release. The flock is bound
  to the file descriptor, so an unclean exit still frees it. The other
  writer should also treat the status file's `phase: syncing`/`conflict`
  as the reverse signal — "gitloop is mid-write, don't start one of your
  own".

## Error isolation

Each repository runs in its own goroutine (`Daemon.superviseRepo`). A panic
anywhere in that repository's loop (including the git client factory call)
is recovered in `Daemon.runRepoGuarded` and converted to an error, which
triggers an exponential backoff retry (30s initial, doubling, capped at
10 minutes) rather than crashing the process or affecting other
repositories. `context` cancellation (SIGTERM/SIGINT via
`signal.NotifyContext` in `cmd/gitloop/run.go`) propagates to every
goroutine for a graceful shutdown, checked ahead of the retry path so a
canceled context never triggers a spurious backoff sleep.
