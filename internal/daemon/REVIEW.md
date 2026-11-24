# Daemon Review Notes

- Publish the terminal LSP runtime state and persisted startup failure before
  removing its pending startup marker. Otherwise check can settle while an
  immediate explicit retry still sees `starting` and is silently discarded.
- Preserve LSP startup errors across file changes and hook metadata refresh.
  A stopped failed runtime must never be reported successful without a retry.
- In LSP startup tests, wait for a terminal hook status as well as settled work.
  The startup marker can clear before the ready status is persisted.
