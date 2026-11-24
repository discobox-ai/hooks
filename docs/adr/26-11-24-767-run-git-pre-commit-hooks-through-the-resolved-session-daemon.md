# 26-11-24-767 — Run Git pre-commit hooks through the committer's resolved session daemon, as a synchronous gate outside the file-hook queue

- **Status**: Proposed
- **Date**: 2026-11-24

## Context

Hooks of `type: pre-commit` are discovered and validated, but nothing runs them
at commit time (#10). The facts below are from `3ae3f14`. Paths are the
current layout; #12 moves the packages under `internal/`.

- `HookTypePreCommit` (`types.go:11`) is only used by `Valid()`. The matcher
  only matches file hooks (`Hook.AppliesToFiles`, `types.go:97`,
  `matcher/matcher.go:111`), so a pre-commit hook runs only when a run request
  names it or selects it through `all`.
- The CLI has no install command. `cmd/discobox-hooks/DESIGN.md:82` lists
  `install-pre-commit` as "when implemented". Root `DESIGN.md` § Pre-Commit
  Integration asks for explicit installation and a generated Git hook that
  calls the CLI/daemon instead of writing status from shell (root `REVIEW.md`).
  It leaves session identity open, and so does the removed `PLAN.md`.
- Session resolution is the same for every command: `--session-id`, then
  `DISCOBOX_SESSION_ID`, then a hash of the repository root
  (`resolveSessionID`, `cmd/discobox-hooks/main.go:2023`). Each session/repository
  pair has its own daemon, socket and SQLite DB (`computeSessionPaths`,
  `main.go:2045`).
- `run` only asks for a run and returns (`main.go:262`). `check` waits on `GET /wait`,
  which settles on *all* daemon work: running and queued hooks, pending
  changes and LSP (`daemon/daemon.go:1332`). It reports every failed hook,
  file hooks included.
- Without `--force`, the daemon skips a run request for a hook whose last run
  succeeded (`already_succeeded`, `service/service.go:322`). A commit always has
  new content, so the gate can't use that rule.
- When a script run fails, every other queued hook is blocked
  (`store/store.go:642`, inside `FinishRun`), and blocked rows never launch
  (`NextPendingExcluding`, `store/store.go:496`). A run that goes through the
  queue can be held up behind an unrelated failed file hook, and a failed
  gate run held in the queue would block the session's file hooks.
- Hooks run with `cmd.Dir = RepoRoot` (`runner/runner.go:136`), against the
  working tree. `DISCOBOX_WORKSPACE` is the repository root unless a request
  sets `Workspace` (`runner/runner.go:207`). The daemon already builds trees
  from a private `GIT_INDEX_FILE` without touching the user's index
  (`daemon/snapshot.go:92`).
- When the CLI starts a daemon, the daemon inherits the CLI's whole
  environment (`cmd.Env = append(os.Environ(), …)`,
  `cmd/discobox-hooks/process_unix.go:65`, `process_windows.go:54`). Inside a
  Git hook, that environment contains `GIT_INDEX_FILE` (a temporary index under
  `git commit -a` or `git commit <paths>`), `GIT_DIR` and related variables.

The design has to settle four questions: which session runs the gate, what
the hooks see, how the commit waits, and how the Git hook is installed.

## Decision

### 1. Session: the one the committer's environment resolves to

The generated Git hook calls `discobox-hooks pre-commit`. That command
resolves the session the same way every other command does: `--session-id`,
then `DISCOBOX_SESSION_ID`, then the repository-root hash. The installed hook
never contains a session ID. It lives in the repository's hooks directory,
which all sessions and worktrees share, and a baked ID would send every
committer to whichever session installed it.

So an agent committing inside its session gates through that session's daemon.
The results show up in the agent's own `ls`, `runs` and `events`, next to the
file hooks it is already watching. A commit from a plain terminal uses the
repository's default session. Each Git worktree resolves its own root, so it
gets its own daemon, as it does for every other command.

The committer's environment says which session it is, and no other source is
trusted for that. The gate never looks for a different live session to run in.

### 2. What the hooks see: the working tree, told exactly what is staged

Gate runs execute in the working tree, as every other hook does. This is where
the tools hooks call (linters, `go vet`, test runners) find their
dependencies, build caches and `node_modules`. The request tells the hooks what
is being committed:

- `DISCOBOX_CHANGED_FILES` / `_JSON`: the staged paths (`git diff --cached
  --name-only -z` against the index Git gave the hook, including deletions),
  so a hook can limit itself to what is committed;
- `DISCOBOX_INDEX_TREE`: a new contract variable, the tree ID that
  `pre-commit` writes from the committer's index (`git write-tree`, run in the
  hook's environment so it sees the temporary index under `commit -a`). A hook
  that needs the exact staged bytes reads them with `git show <tree>:<path>`,
  `git diff <tree>`, or `git archive <tree>`.

If a staged path also has unstaged edits, the hooks can see content that won't
be committed. The CLI prints a warning naming those paths and goes on; it
doesn't fail the commit.

The daemon must not run with Git's hook-time environment. `ensureDaemon`
removes `GIT_INDEX_FILE`, `GIT_DIR`, `GIT_WORK_TREE`, `GIT_PREFIX` and the
other variables Git exports into hooks from the environment of a daemon it
starts. Otherwise a daemon started by a commit would keep running its
background Git calls against a temporary index that no longer exists. The
gate request carries everything the hooks need about the commit (paths and
tree), so the daemon and hook processes never read Git's hook variables.

### 3. Blocking: a synchronous gate call, outside the file-hook queue

A new operation in the OpenAPI contract, `POST /pre-commit`, takes the staged
paths and the index tree. It returns once every selected pre-commit hook has
finished, with each hook's run ID, status and exit code. It works like `/wait`:
the request stays open with a `timeout_seconds`, and when the CLI is cut off
it reconnects and waits on the same gate (the same idea as `waitForCheck`).
The daemon:

- selects every discovered `type: pre-commit` script hook. A hook with a
  `pattern` runs only when a staged path matches it, using its own and the
  global ignore lists. A hook with no `pattern` always runs. `phase` is
  ignored for these hooks: the commit is the trigger.
- always runs them. The `already_succeeded` / `already_ran` skip rules don't
  apply, since each commit is new input.
- runs them outside the pending queue. A blocked file-hook queue doesn't hold
  them up, and a failed gate run doesn't block the queue. They still get run
  rows, logs, `hook.run.*` events and hook status like any other run. Two
  gate runs of the same hook never overlap. A second commit's gate waits for
  the first.
- skips a hook that is paused (globally or per hook) and reports it as
  skipped. Pausing in the daemon means "don't run this", whoever asks.

`discobox-hooks pre-commit` prints each failed hook's output the way `check`
does. It exits non-zero, failing the commit, when a hook fails or the gate
times out (default 10 minutes, `--timeout` overrides it). It also fails
closed when it can't reach or start the daemon: a gate that passes whenever
the daemon is down doesn't gate anything. Every failure message names `git
commit --no-verify` as the escape hatch.

`check` and `GET /wait` don't change. `check` can still report a failed
pre-commit hook, because the status is shared, but the commit never waits on
file hooks.

The gate keeps an honest committer from committing broken code by mistake. It
isn't a security boundary: `--no-verify`, pausing a hook, or editing
`.discobox/hooks` all get around it, and that's fine.

### 4. Install shape

- `discobox-hooks install-pre-commit` writes the hook to the path from `git
  rev-parse --git-path hooks/pre-commit`. That path follows `core.hooksPath`
  and is shared by every worktree of the repository, as Git's own hooks are.
- The generated file is a short POSIX `sh` script (Git for Windows runs hooks
  through its own `sh`). It has a marker line identifying it as ours and holds
  no logic beyond finding the binary: it runs the absolute executable path
  recorded at install time, falls back to `discobox-hooks` on `PATH`, and
  otherwise exits non-zero with a message to reinstall or use `--no-verify`.
  The script never writes status or reads the DB.
- If a pre-commit hook already exists without our marker, the command refuses
  and leaves the file alone. The error names the file and the one line to add
  to that hook (`discobox-hooks pre-commit || exit $?`) to run ours from it.
  There is no `--force`. Running the command again over our own hook rewrites
  it, which also updates the recorded binary path.
- If `core.hooksPath` points inside the work tree (a tracked hooks directory),
  the command writes there and says so, because the result is a file the user
  will commit.
- `discobox-hooks uninstall-pre-commit` removes the hook only when it carries
  our marker. Otherwise it refuses.
- Installing is always explicit. The daemon never installs a hook, and neither
  does any other command.

Once implemented, root `DESIGN.md` § Pre-Commit Integration and
`cmd/discobox-hooks/DESIGN.md` describe this as current state, and
`DISCOBOX_INDEX_TREE` joins the hook environment contract.

## Alternatives rejected

- **A dedicated per-repository pre-commit session.** This keeps every commit in
  one place, but it starts a second daemon per repository (with its own
  watcher, snapshots and DB) just for commits. An agent's commit failures would
  then land somewhere the agent isn't watching. It also handles a committer
  differently from every other command, for no gain in correctness.
- **Finding a live session's daemon for the repository, then falling back.**
  When two sessions are live in one repository, choosing one is a guess.
  Results would land in a session that didn't commit, and the choice would
  depend on which daemons happened to be idle-shut-down. Scanning
  `$XDG_RUNTIME_DIR` for other sessions' sockets also breaks session isolation
  (root `REVIEW.md`).
- **Recording the installing session in the hook.** Every later committer,
  including other sessions, would run in the installer's session, even after it
  has ended.
- **Running against the staged content through `git stash --keep-index`.** This
  rewrites the user's working tree during the commit. The session's own
  watcher would see the edits and queue file hooks, any agent editing at the
  same time would race it, and a crash between stash and pop loses work.
- **Running against a checkout of the index in a temporary directory.** This
  is exact, but it copies the whole tree on every commit, and it runs hooks
  without the dependencies, build caches and untracked config they rely on.
  Hooks that need exact bytes get `DISCOBOX_INDEX_TREE` instead.
- **A filter on `check` (or `/wait`).** `/wait` settles on daemon-wide work and
  `run` goes through the queue. Filtering what is waited on still leaves gate
  runs blocked behind a failed file hook, held back by the `already_succeeded`
  skip, and able to block file hooks when they fail. The commit needs different
  run semantics, not a narrower wait.
- **`run --phase pre-commit` plus `check` from the shell script.** This has the
  same queue problems. It also puts the decision about what blocks a commit in
  shell, which root `REVIEW.md` rules out.
- **Failing open when the daemon is unreachable.** A gate that passes whenever
  its daemon is down is a gate in name only. `--no-verify` is always there as
  an explicit escape hatch.
- **Overwriting or chaining an existing hook automatically.** This would
  destroy or reorder hooks that some other tool owns (husky, pre-commit,
  lefthook) without the user knowing. Calling one documented line from their
  hook is explicit and composes with any of them.

## Consequences

- A commit's gate result is in the committer's session history and event feed,
  like any other run, with no new state store.
- The OpenAPI contract gains `POST /pre-commit` and the generated server and
  client code that goes with it. The hook environment contract gains
  `DISCOBOX_INDEX_TREE`, and `api.KnownEventTypes` gets any new gate events.
- The daemon gets a second way to run hooks, beside the queue. It has to keep
  the same per-hook rule (no two runs of one hook at once), write run rows
  through a finish path that doesn't block the queue, and stop with the daemon.
- `ensureDaemon` gets an environment scrub, which also covers a daemon started
  by any command run from inside a Git hook.
- A hook author has to read `DISCOBOX_INDEX_TREE` to check exactly what is
  committed. A hook that reads the working tree can pass on unstaged edits; the
  warning about partially staged paths makes that visible.
- Repositories that already use another hook manager don't get our hook
  installed. Their users add one line to their own hook instead.

## Deferred

Implementation settles these without coming back to this ADR:

- **Gate concurrency.** Do gate runs count toward `MaxParallelHooks` or run
  one at a time? Decide in implementation, and keep the rule that one hook
  never has two runs at once.
- **Streaming output.** Does `discobox-hooks pre-commit` stream hook output
  live (following `events`), or print it only when the gate ends? Start with
  printing at the end, like `check`. Add streaming if long gates make commits
  look hung.
- **Other Git hooks** (`commit-msg`, `pre-push`). Out of scope. Revisit when a
  hook type for them is proposed; the install shape here should extend to them.
