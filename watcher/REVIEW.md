# Watcher Review Notes

- Mark a skipped path as reported only once its error was actually delivered on
  `Errors`. `sendError` drops an error when the buffer is full, and `New` queues
  every startup failure before anyone reads the channel; a path marked first and
  dropped after is never reported while it keeps failing.
- Keep per-path walk failures below the root non-fatal, and carry the skipped
  subtree's previous entries forward. Returning a walk error drops the whole
  rescan (no change is seen anywhere), and dropping the entries reports every
  file under an unreadable directory as deleted.
