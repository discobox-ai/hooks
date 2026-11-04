---
name: verify-recipes
description: Build and exercise the standalone hooks CLI and its session daemon.
---

Build `go build -o /tmp/discobox-hooks ./cmd/discobox-hooks`.
Use a temporary Git repository (`git init`) containing executable hooks in
`.discobox/hooks`; put XDG_STATE_HOME and XDG_RUNTIME_DIR in separate temporary
directories outside that repository. Run every CLI command with the same
`--repo-root <repo> --session-id <unique-id>` arguments and XDG environment.
The CLI starts its daemon automatically. Always finish with `daemon shutdown`.

For LSP startup failure, define an executable file hook with `engine: lsp`,
`pattern: "**/*.go"`, and `language_id: go` that exits nonzero on startup.
`run <id>` then `check --timeout 3s` must exit nonzero and include the startup
error. Edit a matching file, allow the watcher stability window (~200ms), and
request a script hook run to flush the batch. Repeat check: it must retain the
LSP error and settle with queued scripts still pending. Repair the command to
speak stdio LSP (initialize response with capabilities), explicitly `run <id>`,
and check again: the scripts should finish and check should succeed. Also probe
multiple matching edits after failure and pausing the failed hook.

Capture command output and exit codes. Use isolated sessions and temporary
repositories so verification never runs the checkout's own hooks or modifies its
session database. Remove temporary fixture directories after shutting down.
