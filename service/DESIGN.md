# Service Design

`service` owns API-shaped hooks application operations used by the hook
`manager` and daemon API adapter.

## Responsibilities

- Translate API request DTOs into store operations.
- Return API response DTOs or raw `model` values wrapped by API DTOs.
- Enforce API-level hook behavior such as manual `run` skip/force semantics,
  including skipping a never-run file hook with nothing queued (`nothing_changed`)
  when the request is `expanded`. Failed-run input replay for unforced re-runs
  is owned by the store's enqueue carry-forward.
- Enforce phase targeting for manual runs: phase hooks require a matching
  request phase before they can be enqueued.
- Keep HTTP parsing, status codes, and socket routing in `daemon`.
- Keep file watching, matching, and process execution in their dedicated packages.

## Boundaries

The manager passes the current discovered hook set through a small `HookSet`
interface. The service uses that interface for existence checks, and uses the
store for durable state changes and reads. It does not own queue draining or hook
process execution.
