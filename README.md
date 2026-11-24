# hooks

`hooks` is the standalone Discobox hook runner module.

It discovers hook definitions from `.discobox/hooks` at the Git repository root,
starts a session-scoped daemon on demand, watches file changes, runs matching
hooks with bounded parallelism, and persists hook status and run history through
the shared GORM DB path.

The `discobox-hooks` CLI is in [`cmd/discobox-hooks`](cmd/discobox-hooks).
Programs that drive the daemon import [`pkg/client`](pkg/client) and
[`pkg/api`](pkg/api); [`pkg/hooks`](pkg/hooks) holds the hook model they share.

Design notes live in [`DESIGN.md`](DESIGN.md).
