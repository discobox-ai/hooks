# Store Review Notes

- Preserve lifecycle failures when refreshing LSP metadata, before deleting or
  re-evaluating diagnostic rows. Zero diagnostics do not prove a server started;
  deriving success after refresh can erase a startup error from a stopped server.
