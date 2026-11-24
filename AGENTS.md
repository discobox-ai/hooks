# Repository Guidelines

## Project Structure

- Root module `github.com/discobox-ai/hooks`: the standalone Discobox hook
  runner, a session-scoped daemon that runs `.discobox/hooks` on file changes,
  and the `discobox-hooks` CLI that drives it.
- The root holds only top-level containers (`cmd`, `pkg`, `internal`, `docs`,
  `test`, `scripts`), never packages.
- `cmd/discobox-hooks`: the `discobox-hooks` binary.
- `pkg`: packages other programs may import: `pkg/hooks` (hook model),
  `pkg/api` (socket OpenAPI contract and DTOs) and `pkg/client` (socket client).
- `internal`: packages private to hooks: the daemon and everything it runs on,
  and `internal/cmd/genmodelaliases`, a code generator.
- `docs`: user/developer documentation and ADRs (`docs/adr`).
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes. Read the
  root `REVIEW.md` before changing anything.

## Shared Libraries

Reuse `github.com/discobox-ai/x` packages when applicable instead of writing
local equivalents (`go get github.com/discobox-ai/x@main`). If a generic
helper would be useful beyond hooks, it belongs in `x`, not here.

## Commands

Use Taskfile targets through the Go tool-managed `task` binary:

```bash
go tool task --list
go tool task build      # compile into build/
go tool task test       # run tests (-race)
go tool task check      # golangci-lint, repocheck, shellcheck, actionlint
go tool task generate   # regenerate generated files
go tool task verify     # fmt, go.mod, generated and managed files are current
go tool task ci         # everything CI runs: verify, check, test
```

At the end of a code-changing task, run `go tool task ci`. Add Taskfile
targets instead of documenting ad hoc commands here.

## Implementation Quality

Prefer proper structural changes over compatibility shims or narrow patches.

- Do not introduce optional interfaces for behavior the system requires. Add
  required methods to the core interface and update all implementations.
- Do not add wrapper types, adapter layers, or helper wrappers just to avoid
  touching callers. Change the call sites.
- Treat existing databases and persisted state as durable. Design schema
  changes with a safe upgrade path.
- When the correct fix crosses package boundaries, update the model,
  interfaces, implementations, tests, and call sites together.
- Keep abstractions justified by durable ownership or meaningful complexity
  reduction.

## Package Design Docs

`DESIGN.md` explains the design of a package and its subdirectories.
`REVIEW.md` holds its review notes: what to check and the pitfalls already
hit. Read both from the root down to every package you are changing or
reviewing; closer files override parents. When reviewing, a change that
breaks a `REVIEW.md` note is a finding.

Add a bullet to the closest `REVIEW.md` when you find something non-obvious a
future review should check, when the same mistake is made a second time, or
when you fix a regression. The bullet states the rule, then why. The author
writes it, in the change that fixes what prompted it; a reviewer that thinks
one is due says so in its comment. Keep both files short and directive,
describe current state only, and update them in the same change as the code.
See the `repostd` skill for the rules.

## Architecture Decision Records

`docs/adr` records decisions and the alternatives rejected. Write an ADR only
when a plausible alternative was rejected for a non-obvious reason, or
something was deferred with a condition for revisiting it. Land it as
`Proposed`; `Accepted` is the go-ahead. ADRs are immutable once shipped
against: supersede, never edit. See `docs/adr/README.md`.
