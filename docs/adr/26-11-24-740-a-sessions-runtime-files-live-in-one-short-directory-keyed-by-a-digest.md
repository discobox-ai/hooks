# 26-11-24-740 — A session's runtime files live in one short directory keyed by a digest

- **Status**: Proposed
- **Date**: 2026-11-24

## Context

The CLI derives every session path in `computeSessionPaths`
(`cmd/discobox-hooks/main.go:2045`). The socket, the startup lock, the runtime
metadata (`runtime.json`) and the daemon's `tmp` directory all live in one
runtime directory:

```
<runtime-home>/discobox/session/<session-id>/hooks/<repo-key>/daemon.sock
```

`<runtime-home>` is `$XDG_RUNTIME_DIR`, or `<state-home>/run` when that is
unset (`runtimeHome`, `main.go:2266`). `XDG_RUNTIME_DIR` is unset on every
Windows and macOS machine, and on Linux without a systemd login session
(containers, plain SSH). `<session-id>` is the harness's session ID after
`safePathComponent` (`main.go:2273`), which replaces bytes but bounds nothing;
a UUID is 36 bytes. `<repo-key>` is 16 hex digits of the SHA-256 of the
repository root (`repoStateKey`, `main.go:2242`).

The layout after `<runtime-home>` costs 89 bytes with a UUID session. A socket
path has to fit in `sockaddr_un`. Go refuses anything longer than 108 bytes on
Linux, and anything over 103 bytes on macOS and the BSDs
(`syscall.SockaddrUnix`). Windows binds at most 107 bytes (measured in #1).
So the path overflows once `<runtime-home>` is longer than 19 bytes on Linux,
18 on Windows, or 14 on macOS. Ordinary machines are
well past that:

| Runtime home | Bytes | Socket path today |
|---|---|---|
| `/run/user/1000` (Linux, `XDG_RUNTIME_DIR`) | 14 | 103 (fits, with no margin) |
| `/home/darren/.local/state/run` | 29 | 118 |
| `/Users/darren/.local/state/run` | 30 | 119 |
| `C:\Users\darren\AppData\Local\run` | 33 | 122 |

When the path overflows, the daemon's `listen unix` fails with
`bind: invalid argument` (`daemon/daemon.go:295`). The CLI then waits ten
seconds and reports `daemon did not become ready at <path>`
(`main.go:2110`), which doesn't name the cause. Issue #1's triage reproduces
this on Linux at `3ae3f14`. The tests miss it because they root their sockets
under `x/shorttmp`.

Only the socket is bounded. The lock, `runtime.json`, the `tmp` directory and
the database (`<state-home>/discobox/session/<session-id>/hooks/<repo-key>/hooks.db`)
are ordinary files with no such limit.

Three things in today's code constrain where the socket can go:

- **The socket's directory is the daemon's `tmp` directory's parent.** The CLI
  passes `TempDir` as `<runtime-dir>/tmp` (`main.go:158`). The daemon defaults
  it to `filepath.Dir(SocketPath)/tmp` when a caller doesn't
  (`daemon/daemon.go:145`, `daemon/snapshot.go:146`). The daemon also empties
  that directory with `os.RemoveAll` when it starts (`resetRuntimeTempDir`). A
  socket directory shared by several sessions would let one daemon delete
  another's files.
- **The socket path and the startup lock are what keep a session to one
  daemon.** Nothing locks the database. A client pings the socket, takes the
  lock, pings again, and starts a daemon only when neither ping answers
  (`ensureDaemon`, `main.go:2063`). If two clients compute different socket
  paths for the same database, both start a daemon, and both daemons write the
  same database.
- **Daemon replacement only looks at the computed path.** A newer client shuts
  down an older daemon through the socket it computes (`replaceOlderDaemon`,
  `main.go:2117`). It finds a stale process through `runtime.json` in the
  same directory (`terminateStaleRuntimeDaemon`, `main.go:2146`). A daemon
  that an older build started at the old path is invisible to a newer client
  that computes a new path.

`DESIGN.md` makes the session directory "the root namespace for all future
session-scoped Discobox state". That holds for durable state. The runtime
fallback already sits in a separate tree (`<state-home>/run/...`), and deleting
a session's state directory leaves it behind today.

## Decision

1. **A session's runtime directory is short and keyed by a digest.** Per
   session and repository, it is

   ```
   <runtime-home>/discobox/hooks/<runtime-key>/
   ```

   and it holds what the runtime directory holds today: `daemon.sock`,
   `startup.lock`, `runtime.json` and `tmp/`, under the same names.
   `<runtime-key>` is the first 16 hex digits of the SHA-256 of the session
   component, then a NUL byte, then the cleaned repository root. The session
   component is the session ID after `safePathComponent`, which is the string
   that names the database directory. Two sessions that share a database
   therefore share a runtime directory, and so a daemon. Hashing the raw ID
   instead would give them two daemons on one database.

   This leaves 44 bytes after `<runtime-home>`. The socket fits when
   `<runtime-home>` is at most 59 bytes on macOS, 63 on Windows, or 64 on
   Linux. The table above becomes 58, 73, 74 and 77 bytes.

2. **`<runtime-home>` is unchanged.** It is `$XDG_RUNTIME_DIR`, or
   `<state-home>/run` when that is unset.

3. **The database stays where it is**, at
   `<state-home>/discobox/session/<session-id>/hooks/<repo-key>/hooks.db`.
   The session-scoped namespace applies to durable state, which is where it
   already applies.

4. **An overlong socket path is reported, not attempted.** After computing the
   paths, the CLI checks the socket path against the platform's limit before
   it connects or starts anything: 108 bytes on Linux, 107 on Windows, and
   103 on macOS and the BSDs. When the path is too long, the command fails at once.
   The error names the socket path, its length and the limit, and says that
   setting `XDG_RUNTIME_DIR` to a shorter directory fixes it. The daemon makes
   the same check before it listens, so a daemon started some other way fails
   the same way. A long `XDG_RUNTIME_DIR` or a long home directory produces
   this error. It doesn't produce a silent fallback.

5. **A daemon started by an older build at the old path is shut down.** Under
   the startup lock, before it starts a daemon, a client also tries the legacy
   runtime directory
   (`<runtime-home>/discobox/session/<session-id>/hooks/<repo-key>/`). If a
   daemon answers on the legacy socket, the client asks it to shut down and
   waits, as `replaceOlderDaemon` does today. If a live PID is recorded in the
   legacy `runtime.json`, the client terminates it, as
   `terminateStaleRuntimeDaemon` does today. The client only probes a legacy
   socket path that fits the platform limit, since no daemon can be listening
   on one that doesn't. The legacy directory is left on disk.

6. **`DISCOBOX_SOCKET_PATH` keeps its meaning.** Hooks get the new socket path
   in it. The variable's name and contract don't change.

## Alternatives rejected

- **Only check the length and keep the layout.** This turns a ten-second
  timeout into a clear error, but the error would hit every Windows and macOS
  machine and most Linux containers. It moves the failure without fixing it.
  The check is kept, as step 4, for the cases the short layout still can't
  fit.
- **A short socket file in one directory shared by all sessions**
  (`<runtime-home>/discobox/hooks/<runtime-key>.sock`), with the lock and
  metadata left in the per-session tree. It saves 7 bytes. But the daemon
  derives its `tmp` directory from the socket's directory and empties it when
  it starts. Sessions sharing that directory would delete each other's
  temporary files, unless every caller passed `TempDir`, and nothing enforces
  that. It also splits a daemon's runtime files across two trees, and the
  socket, lock and metadata always change together.
- **Bound the session component and keep the tree** (truncate or hash
  `<session-id>` to something short). The tree stays deep: a 16-byte session
  component leaves 69 bytes after `<runtime-home>`. That still overflows
  under `%LOCALAPPDATA%` for a Windows username longer than 11 bytes, under
  `~/.local/state` for a macOS one longer than 10, and under any
  `XDG_RUNTIME_DIR` deeper than 39 bytes. To
  agree with the runtime path, the database path would have to change too,
  which moves every user's existing hook history.
- **Fall back to a short system temp directory** (`/tmp`, `C:\`) when the
  socket path is too long. On Unix, `/tmp` is shared by every user. A socket
  or directory there can be pre-created or replaced by another account, unless
  the code takes on owner and mode checks of its own. A per-user directory
  that a person can set (`XDG_RUNTIME_DIR`) is the safer remedy, so step 4
  points to it instead.
- **Named pipes on Windows instead of AF_UNIX.** A pipe name has no 108-byte
  limit. But the client, daemon and tests would need a second transport (Go's
  standard library has no named-pipe listener, so a dependency too), and the
  socket would still overflow on macOS and Linux.
- **An abstract-namespace socket on Linux.** It has the same length limit, it
  exists only on Linux, and it has no file permissions to restrict who
  connects.

## Consequences

- With an ordinary home directory, the daemon starts on Windows, on macOS and
  on Linux without `XDG_RUNTIME_DIR`. Where it can't, the failure is
  immediate and says how to fix it.
- `DESIGN.md` and `cmd/discobox-hooks/DESIGN.md` must describe the new runtime
  path and the length check. Their "socket, startup lock, and runtime
  metadata" lines change.
- The runtime directory's name no longer shows the session or the repository.
  `runtime.json` records both (`session_id`, `repo_root`), so the directory
  can still be identified.
- A test like the triage's `TestSessionSocketFitsUnderOrdinaryStateHome`, with
  a 25- to 33-byte state home and no `XDG_RUNTIME_DIR`, must pass on every
  platform CI runs.
- An older client binary run in the same session after an upgrade still
  computes the old path. It can start a second daemon on the same database.
  This is the same exposure the version-replacement check already accepts for
  mixed builds. The replacement only guards an older daemon, not an older
  client.
- Script hooks still don't run on Windows (#18), and the Windows startup lock
  doesn't lock (#19). This decision doesn't change either. Both block "works on
  Windows", independently of the socket path.

## Open (for implementation)

- When to remove the legacy-path probe in step 5. One release after this
  ships is the expected answer, but it isn't decided here.
- Whether the daemon's `TempDir` default (`daemon/daemon.go:145`) should
  become required. It is safe under this layout because the socket's directory
  is per session again, so this ADR doesn't require changing it.
