# Hooks Review Notes

- Keep `pkg/` free of `internal/` imports, so nothing exported there names an
  internal type that programs outside this module cannot use.
  `TestPublicPackagesImportNothingInternal` guards it. It was broken once by
  `pkg/api/model` exposing `internal/models.Status` when the packages moved
  into `pkg/` and `internal/` (#4).
- Keep this module standalone. Do not import server internals, control-plane DTOs,
  or Discobot `harness-go` packages.
- Preserve session scoping: one daemon/socket/database namespace per session.
- Keep daemon-owned DB writes authoritative. CLI commands should use the Unix
  socket API for mutations.
- Use `gormdb` for DB opening and keep migrations/models compatible with SQLite
  first.
- Preserve daemon-configured bounded parallel hook execution. A failed hook must
  block future queued hook launches; hooks already in flight may finish.
- Treat `.discobox/hooks` as repository-root relative. Resolve and validate the
  Git root before discovery or watching.
- Respect Git ignored files before matching hooks.
- Keep native AI execution out of the initial implementation. AI behavior should
  be represented as script hooks calling external tools.
- Avoid shell-side database mutation for generated Git hooks; route through the
  CLI/daemon.
- Keep audit event docs current. `api.KnownEventTypes` is the user-facing event
  catalog for `discobox-hooks events --list-types`; add every new production
  `recordEvent`/`hook_events` type there with a description. The
  `TestKnownEventTypesDocumentProductionEvents` CLI test guards daemon/store
  event literals.
- Update `DESIGN.md` for architecture or data model changes. It describes
  current behaviour only; plans and open questions belong in issues.
