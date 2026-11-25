# Daemon Review Notes

- Publish the terminal LSP runtime state and persisted startup failure before
  removing its pending startup marker. Otherwise check can settle while an
  immediate explicit retry still sees `starting` and is silently discarded.
- Preserve LSP startup errors across file changes and hook metadata refresh.
  A stopped failed runtime must never be reported successful without a retry.
- In LSP startup tests, wait for a terminal hook status as well as settled work.
  The startup marker can clear before the ready status is persisted.
- Never remove the daemon socket by path on exit. The listener unlinks the file
  it bound when it closes, and `http.Server.Shutdown` closes it before draining
  connections, so a later path-based remove can delete a newer daemon's live
  socket. A daemon that never bound the socket must not touch it either.
