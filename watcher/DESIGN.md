# Watcher Design

The watcher package observes a Git worktree and emits stable filesystem change
batches. It should hide low-level OS event noise from the daemon while preserving
created, modified, and deleted path information.

## Responsibilities

- Watch the Git root recursively.
- Ignore `.git`, `node_modules`, and other paths that should never trigger hook
  work.
- Convert OS-specific events into repository-relative slash paths.
- Debounce kernel/editor noise into stable watcher batches.
- Detect created, modified, and deleted entries.
- Recover from watcher overflow by rescanning the tree.
- Expose an event channel and error channel with clean shutdown semantics.

## Snapshot-Diff Model

Use the Discobot `harness-go/filewatcher` design as the starting point:

1. Maintain a snapshot of watched paths.
2. Use native file notifications as a wake-up signal.
3. After a short debounce, rescan the tree.
4. Diff old and new snapshots.
5. Emit semantic changes rather than raw OS events.

This model handles atomic writes, rename bursts, new nested directories, and
missed raw events better than forwarding inotify events directly.

## Public Concepts

- `Watcher`: owns OS resources and emits batches.
- `Options`: debounce, initial snapshot, periodic resync, ignore controls.
- `Batch`: a set of changes, optional resync marker, and the full post-diff
  snapshot when changes were found.
- `Change`: path, kind, and optional entry metadata.
- `Entry`: file/dir metadata needed to detect stable changes.

## Platform Strategy

Use `fsnotify` on every platform supported by that dependency instead of hiding
the watcher behind OS build tags. Treat native notifications as wake-up signals
only; the portable contract comes from the rescan + snapshot-diff layer. This
keeps behavior stable across Linux, macOS, BSD, Windows, and other supported
targets even when low-level event details differ.

## Relationship to Matcher

Watcher-level ignore support prunes universally noisy directory trees such as
`.git` and `node_modules`. The daemon filters `.gitignore`-ignored changes before
persisting observed changes, and the matcher still enforces Git ignore and hook
pattern semantics as the final policy gate.

## Restart Catch-Up

The watcher accepts an `InitialSnapshot` so the daemon can seed it from the last
persisted `watched_files` checkpoint. On the first periodic resync after restart,
the watcher compares that persisted snapshot to the current tree and emits
created, modified, and deleted changes that happened while the daemon was
stopped. Emitted batches include the new full snapshot so the daemon can replace
the checkpoint after processing.

Regular files are compared by size and content hash, and the checkpoint carries
each entry's `Hash` so a restart compares content too: a file rewritten with
identical bytes while the daemon was stopped is not reported. When either side
of a comparison has no hash (a checkpoint row from an older build, or a file that
could not be read), the comparison falls back to size and mtime.
`BackfillHashes` upgrades such a checkpoint: it hashes each hashless file whose
size and mtime still match its entry (the files that fallback would call
unchanged anyway) so the daemon can save them once. A scan reuses
the previous hash when a file's size and mtime are unchanged, so an edit that
keeps both is not detected, at restart as in steady state.

## Unreadable Paths

Only a failure at the watch root is fatal: `New` returns it, and a resync that
cannot read the root reports it and keeps the previous snapshot. Below the
root, a directory that cannot be read or watched (a mode-0 directory, a
root-owned mount), or an entry that cannot be stat'ed, is pruned from that walk
and the rest of the tree is still scanned and watched, so changes elsewhere
still arrive as batches.

- The pruned subtree's entries are carried forward from the previous snapshot.
  What the watcher cannot see is treated as unchanged, not deleted, so a
  directory losing its read permission does not emit a delete for every file
  in it. Once it is readable again the next resync diffs it normally.
- Each skipped path is sent on `Errors` as an error naming that path (fsnotify's
  own watch error does not, so the watcher wraps it). The daemon resyncs every
  second, so a path is reported when it first fails and not again while it
  keeps failing; once a full pass no longer fails on it, it is forgotten and
  reported again if it fails later. An error dropped because `Errors` was full
  does not count as reported, so a later pass sends it again.
- Reporting stops at the `Errors` channel. The daemon reads and discards it
  today, so a skipped path is not yet visible to the user; surfacing watcher
  errors is the daemon's to do.

## Non-Responsibilities

- Do not know about hook definitions.
- Do not enqueue or execute hooks.
- Do not write status to the database.
- Do not implement the five-second hook scheduling debounce; that belongs to the
  daemon scheduler above watcher batches.
