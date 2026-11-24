package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/hooks/models"
	"github.com/discobox-ai/hooks/watcher"
	"gorm.io/gorm"
)

// A watcher seeded from the persisted checkpoint of an unchanged tree must not
// report any change: this is what a daemon restart does.
func TestRestoredWatchedSnapshotReportsNoChangesForUnchangedTree(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	persistFirstSnapshot(ctx, t, s, root)

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	second, err := watcher.New(root, watcher.Options{InitialSnapshot: restored, PeriodicResync: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("second watcher: %v", err)
	}
	defer second.Close()

	select {
	case batch := <-second.Batches():
		t.Fatalf("restart over an unchanged tree reported %d changes, want 0: %+v", len(batch.Changes), batch.Changes)
	case <-time.After(300 * time.Millisecond):
	}
}

// Rows saved by a build that did not persist hashes have none. The first
// restart after upgrading must not report every such file as modified.
func TestRestoredHashlessWatchedSnapshotReportsNoChangesForUnchangedTree(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	persistFirstSnapshot(ctx, t, s, root)
	clearWatchedHashes(ctx, t, s)

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	for path, entry := range restored {
		if entry.Hash != "" {
			t.Fatalf("restored %s with hash %q, want none", path, entry.Hash)
		}
	}
	second, err := watcher.New(root, watcher.Options{InitialSnapshot: restored, PeriodicResync: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("second watcher: %v", err)
	}
	defer second.Close()

	select {
	case batch := <-second.Batches():
		t.Fatalf("restart over an unchanged tree from hashless rows reported %d changes, want 0: %+v", len(batch.Changes), batch.Changes)
	case <-time.After(300 * time.Millisecond):
	}
}

// An edit made while the daemon was stopped is still reported on restart.
func TestRestoredWatchedSnapshotReportsContentChangedWhileStopped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	persistFirstSnapshot(ctx, t, s, root)

	path := filepath.Join(root, "pkg/a.go")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte("CONTENT OF pkg/a.go")
	if len(edited) != int(info.Size()) {
		t.Fatalf("edit must keep the size: %d != %d", len(edited), info.Size())
	}
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	second, err := watcher.New(root, watcher.Options{InitialSnapshot: restored, PeriodicResync: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("second watcher: %v", err)
	}
	defer second.Close()

	select {
	case batch := <-second.Batches():
		if len(batch.Changes) != 1 || batch.Changes[0].Path != "pkg/a.go" || batch.Changes[0].Kind != watcher.Modified {
			t.Fatalf("restart reported %+v, want one modified pkg/a.go", batch.Changes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not report the file edited while stopped")
	}
}

// The checkpoint keeps each regular file's hash, so a restart compares content.
func TestLoadWatchedSnapshotKeepsHashes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	want := persistFirstSnapshot(ctx, t, s, root)

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	for path, entry := range want {
		if !entry.Mode.IsRegular() {
			continue
		}
		if entry.Hash == "" {
			t.Fatalf("watcher left %s without a hash", path)
		}
		if got := restored[path].Hash; got != entry.Hash {
			t.Fatalf("restored %s hash = %q, want %q", path, got, entry.Hash)
		}
	}
}

// A file rewritten with identical bytes and a new mtime while the daemon was
// stopped (go mod tidy, a formatter with no diff) is not a change: only the
// persisted hash can tell, since its mtime moved.
func TestRestoredWatchedSnapshotIgnoresIdenticalRewriteWhileStopped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	persistFirstSnapshot(ctx, t, s, root)
	rewriteIdentically(t, filepath.Join(root, "pkg/a.go"))

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	assertNoRestartChanges(t, root, restored)
}

// A checkpoint from an older build is upgraded the way the daemon does at
// startup: backfilled hashes are saved, and afterwards an identical rewrite of
// an untouched file is not a change.
func TestBackfilledWatchedSnapshotIgnoresIdenticalRewriteWhileStopped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	root := writeRestartTree(t)
	persistFirstSnapshot(ctx, t, s, root)
	clearWatchedHashes(ctx, t, s)

	legacy, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if filled := watcher.BackfillHashes(root, legacy); filled != 3 {
		t.Fatalf("backfilled %d hashes, want 3", filled)
	}
	if err := s.ReplaceWatchedSnapshot(ctx, legacy); err != nil {
		t.Fatalf("save backfilled snapshot: %v", err)
	}
	rewriteIdentically(t, filepath.Join(root, "pkg/a.go"))

	restored, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	assertNoRestartChanges(t, root, restored)
}

func assertNoRestartChanges(t *testing.T, root string, restored map[string]watcher.Entry) {
	t.Helper()
	w, err := watcher.New(root, watcher.Options{InitialSnapshot: restored, PeriodicResync: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("restarted watcher: %v", err)
	}
	defer w.Close()
	select {
	case batch := <-w.Batches():
		t.Fatalf("restart reported %d changes, want 0: %+v", len(batch.Changes), batch.Changes)
	case <-time.After(300 * time.Millisecond):
	}
}

func rewriteIdentically(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// clearWatchedHashes puts the checkpoint in the state an upgrade leaves it in:
// AutoMigrate adds the hash column to rows that had none, as NULL.
func clearWatchedHashes(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	if err := s.write.WithContext(ctx).Model(&models.WatchedFile{}).Where("1 = 1").Update("hash", gorm.Expr("NULL")).Error; err != nil {
		t.Fatalf("clear hashes: %v", err)
	}
}

func writeRestartTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", "pkg/a.go", "pkg/b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func persistFirstSnapshot(ctx context.Context, t *testing.T, s *Store, root string) map[string]watcher.Entry {
	t.Helper()
	first, err := watcher.New(root, watcher.Options{})
	if err != nil {
		t.Fatalf("first watcher: %v", err)
	}
	defer first.Close()
	snap := first.Snapshot()
	if err := s.ReplaceWatchedSnapshot(ctx, snap); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	return snap
}
