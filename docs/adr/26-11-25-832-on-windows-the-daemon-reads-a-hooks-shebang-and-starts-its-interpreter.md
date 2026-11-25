# 26-11-25-832 — On Windows, the daemon reads a hook's shebang and starts its interpreter

- **Status**: Proposed
- **Date**: 2026-11-25

## Context

A hook file is a script with a shebang, front matter, and a body
(`parser/DESIGN.md`, Hook File Shapes). The parser requires the shebang for
both engines that start a process: `engine: script` and `engine: lsp`
(`parser/parser.go:311`, `:319`). It checks the executable bit everywhere
except Windows (`:314`, `:322`), so on Windows a hook passes discovery.

Both places that start a hook hand the file's path to the OS as the program:

- **Script hooks.** `runner.Run` resolves `Hook.Command` against the
  repository root and calls `exec.CommandContext(ctx, cmdPath, Args...)`
  (`runner/runner.go:135`). The daemon fills `Command` with the hook's
  absolute path (`runnerHookDefinition`, `daemon/daemon.go:1200`).
- **LSP hooks.** `lspclient.Start` passes the hook's path as
  `CommandOptions.Command` to `processhelper.CommandContext`
  (`lspclient/client.go:119`). That re-execs the daemon binary as a helper.
  The helper then runs `exec.CommandContext(…, childArgs[0], …)`
  (`processhelper/helper.go:84`).

On Unix the kernel reads the shebang and starts the interpreter. Windows
doesn't. `CreateProcess` only starts PE images, so every hook fails to start
with `%1 is not a valid Win32 application`. A script hook records exit 126
(`runner.ExitCodeStartup`). An LSP hook's helper exits 126 with
`start child: …`. There is no workaround: every hook file has to open with
`#!` and front matter, so it can't be a `.exe`, `.cmd` or `.ps1`.

Because of this, the tests that need a hook to run skip on Windows:
`runner/runner_test.go:53,83` and `daemon/daemon_test.go:43`
(`requireExecutableHooks`, called by five daemon tests and by the LSP
startup test in `daemon/lsp_startup_test.go:67`). The owner decided on 2026-11-24 (#1) that
discobox-hooks targets Windows too.

A repository's `.discobox/hooks` directory is checked in and shared by every
platform its developers use. The hooks in it today are written for Unix:
`#!/bin/sh`, `#!/bin/bash`, `#!/usr/bin/env bash`, and
`#!/usr/bin/env python3` (or another tool). On Windows those interpreters come
from:

- **Git for Windows**, which ships `sh.exe` and `bash.exe` in `<git>\usr\bin`
  (and `bash.exe` in `<git>\bin`). Its installer's default puts only
  `<git>\cmd` on `PATH`, so `sh` isn't on `PATH` in `cmd.exe` or PowerShell.
  It is on `PATH` inside Git Bash.
- **`PATH`**, for tools installed natively: `python`, `node`, `pwsh`, or an
  MSYS2 or Cygwin `bash`.
- **`%SystemRoot%\System32\bash.exe`**, the WSL launcher. It is on every
  `PATH` where WSL is installed. It runs the script inside a Linux VM, where
  the Windows paths and the environment the runner builds don't carry over.

Two more facts constrain the design:

- **The two spawn sites differ.** The runner starts the hook itself. The LSP
  client starts a helper that starts the hook. `processhelper/DESIGN.md` keeps
  hook semantics out of the helper.
- **Process trees aren't killed on Windows.** `runner/process_windows.go` and
  `processhelper/process_windows.go` kill only the direct child. Once an
  interpreter sits between the daemon and the hook's own commands, a timeout
  kills `sh.exe` and leaves what it started. That gap exists today for any
  Windows child. This decision makes it reachable.

## Decision

1. **On Windows, the daemon reads the shebang and starts the interpreter
   itself.** The command it starts is the interpreter, then the shebang's
   argument if there is one, then the hook's absolute path, then the hook's
   arguments. This is what the Linux kernel would run:

   ```
   #!/bin/sh -e        →  <sh.exe> -e C:\repo\.discobox\hooks\lint.sh
   #!/usr/bin/env node →  <node.exe> C:\repo\.discobox\hooks\lint.js
   ```

   The working directory, the environment (`runner.BuildEnv`), output capture,
   and timeout and exit-code handling don't change. The interpreter is the
   process whose exit status is recorded, as the script's own exit status is
   on Unix.

2. **The shebang line is parsed as Linux parses it.** Drop the `#!`. Strip a
   trailing `\r`, so a file checked out with CRLF line endings still names
   its interpreter. Trim spaces and tabs. The interpreter is the text up to the
   first space or tab. The rest, trimmed, is one optional argument, and it is
   not split further. A shebang with no interpreter is a startup error.

3. **The interpreter is resolved by its base name.** Windows has no `/bin`,
   so the directory in an absolute POSIX path carries no information:
   `/bin/sh`, `/usr/bin/sh` and `/usr/local/bin/sh` all resolve as `sh`.
   `/usr/bin/env` (or any path whose base name is `env`) is
   replaced by its argument: `#!/usr/bin/env python3` resolves `python3`. An
   `env` shebang whose argument starts with `-` (`env -S …`) is a startup
   error that names the form. An interpreter given as a Windows path
   (`#!C:\Python312\python.exe`) is used as written if it exists.

   A base name is looked up in this order. The first hit wins:

   1. `PATH`, with `PATHEXT` (`exec.LookPath`), skipping any hit inside the
      Windows system directory (`%SystemRoot%\System32`). That skip is what
      keeps the WSL `bash.exe` from being chosen.
   2. Git for Windows, found from `git.exe` on `PATH`: its install root's
      `usr\bin` and then `bin`. A Git for Windows install is a prerequisite
      for a shell hook on Windows. It is where the hooks this repository and
      its users write today get their `sh` and `bash`.

   When nothing resolves, the hook doesn't start. The error names the hook's
   path, the shebang line, and the places searched. For `sh` and `bash` it
   also says that Git for Windows provides them. It is a startup failure like
   any other: a script hook records exit 126 with the error, and an LSP hook
   takes the startup-failure path it already has.

4. **One launcher serves both spawn sites, and it resolves in the parent.**
   A new package exposes one function that maps a hook path and its arguments
   to the program and arguments to start. On Unix it returns them unchanged,
   and the kernel keeps honoring the shebang. On Windows it does steps 2 and 3.
   `runner.Run` calls it before `exec.CommandContext`. `lspclient.Start` calls
   it before `processhelper.CommandContext`, so the helper is handed
   `<interpreter> [arg] <hook path>` and stays free of shebangs and hooks.
   A resolution error surfaces in the daemon, before any process starts. The
   package is the only place that knows the shebang mapping, and it reads only
   the file's first line.

5. **Unix doesn't change.** Hooks are still exec'd by path. The kernel still
   interprets the shebang. The executable bit is still required. Parsing the
   shebang on Unix too would change working hooks (nested interpreters,
   `binfmt_misc`, `env -S`) for no gain there.

6. **Discovery doesn't change.** The parser still requires the shebang on every
   platform and still skips the executable-bit check on Windows. It doesn't
   resolve the interpreter: `PATH` can change between discovery and a run, and
   a missing interpreter on one developer's machine shouldn't hide a hook
   from status. `parser/DESIGN.md` (Engines) documents what step 1 to step 3
   mean on Windows: the shebang forms that run, and that `sh` and `bash` come
   from `PATH` or Git for Windows.

7. **The tests that skip because hooks can't run, run.** On Windows, the
   runner's success and failure tests and the six tests that call
   `requireExecutableHooks` stop skipping. They rely on Git for Windows' `sh`, which GitHub's Windows
   runners have. The launcher gets its own tests for the parsing in step 2,
   run on every platform. It also gets tests for the lookup order in step 3,
   run on Windows: a `PATH` hit, the system-directory skip, the Git fallback,
   and the not-found error. The process-tree tests (`runner_test.go:112`,
   `lspclient/client_test.go:222`, `processhelper/helper_test.go:21`) still
   skip on Windows. Killing a tree there is a separate change (Consequences).

## Alternatives rejected

- **Always run hooks through one shell on Windows** (Git Bash's `bash.exe`,
  or one the user configures). It covers `sh` and `bash` hooks with less
  code. But it ignores what the hook author wrote: a `#!/usr/bin/env python3`
  hook would be fed to `bash` and fail. It also makes a hook behave
  differently on Windows than on Unix, where the shebang decides.
- **Run the hook through `sh` from Git for Windows, and let `sh` honor the
  shebang** (`sh -c 'exec "$0" "$@"' <hook>`). MSYS emulates shebangs, so
  this would start the right interpreter, `/usr/bin/env` included. But
  it makes Git for Windows a prerequisite for every hook, Python and Node ones
  too. It adds a shell process to every run, and it moves the not-found error
  into MSYS's wording. It also depends on MSYS's path mapping for both the
  hook path and the interpreter path. Reading one line ourselves is smaller
  and testable.
- **Require a Windows-native hook form** (`.cmd`, `.ps1` or `.exe` next to
  the shell one, or a per-platform `command:` field). A repository's hooks
  are shared across platforms. Asking every hook author to write and maintain
  two versions is the cost the owner's "works on Windows too" is meant to
  avoid. A front-matter field for the interpreter can still be added later
  without conflicting with this.
- **Run hooks in WSL.** The hook would run in a Linux VM. There, the
  repository's Windows path, the `DISCOBOX_*` paths and the socket aren't the
  paths the hook sees, and WSL often isn't installed. This is also why step 3
  skips the system directory.
- **Resolve the interpreter in the process helper.** The helper would need
  shebang parsing that `processhelper/DESIGN.md` keeps out of it. Its error
  would reach the daemon only as the helper's stderr and exit 126, and the
  runner would still need its own copy.
- **Look in Git for Windows before `PATH`.** It would pick Git's `bash` over an
  MSYS2 or Cygwin one the user chose to put on `PATH`. That breaks the rule
  that what runs is what the user's own shell would find. With the
  system-directory skip, `PATH`-first is safe.
- **Check the interpreter at discovery.** Rejected in step 6.

## Consequences

- A `#!/bin/sh` hook, a `#!/bin/bash` hook, and a `#!/usr/bin/env <tool>` hook
  run on Windows when the interpreter is installed, with output and exit status
  recorded as on Unix. When it isn't, the run fails at once and says what to
  install.
- A hook body has to work under the interpreter Windows has. A `sh` hook runs
  under MSYS, where `/tmp`, `/c/…` and `cygpath` exist, but where Windows
  binaries see Windows paths. A hook that relies on GNU-only tools behaves as
  it would in Git Bash. That's the hook author's concern, not the daemon's.
- A hook checked out with CRLF line endings resolves its interpreter. But
  `sh` or `bash` may still reject a CRLF body (`$'\r': command not found`).
  The daemon doesn't rewrite hook files. A repository that wants shell hooks
  on Windows marks them `eol=lf` in `.gitattributes`.
- On Windows, a hook's timeout or cancellation kills the interpreter but not
  what it started. Those processes can keep the output pipe open, and
  `runner.Run` sets no `WaitDelay`, so the run may not return until they exit.
  This is the same gap any Windows child has today. Fixing it means a job
  object per run in `runner` and `processhelper`. That's a separate issue, and
  it doesn't block hooks running. Until it's fixed, the process-group tests
  keep skipping on Windows.
- `runner/DESIGN.md` ("Execute script hooks from the Git root"),
  `processhelper/DESIGN.md` (its child is an interpreter on Windows) and
  `parser/DESIGN.md` (Engines) are updated with the implementation.
- "Passes on Windows CI" depends on the Windows CI job that #12 adds.

## Open (for implementation)

- Whether the hook path is handed to the interpreter with backslashes
  (`C:\repo\…`) or forward slashes (`C:/repo/…`). Both MSYS and native
  interpreters accept either. Windows CI decides which one the tests pass with.
- Whether to support `env -S` by splitting on whitespace, as GNU `env` does.
  This ADR rejects it with a clear error until a hook needs it.
- Whether to add a variable that overrides where `sh` and `bash` come from
  (as some tools do with a Git Bash path). Not needed while `PATH` and Git for
  Windows cover the installs we know of.
- The new package's name and whether it also returns the parsed shebang for
  status output.
