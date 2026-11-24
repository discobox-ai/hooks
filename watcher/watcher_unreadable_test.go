package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatcherSeesChangesOutsideUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	w := newTestWatcher(t, root)
	defer w.Close()

	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	writeFile(t, filepath.Join(root, "app.go"), []byte("package app\n"))

	var errs []error
	deadline := time.After(2 * time.Second)
	for {
		select {
		case batch := <-w.Batches():
			for _, c := range batch.Changes {
				if c.Path == "app.go" && c.Kind == Created {
					for _, err := range errs {
						if !strings.Contains(err.Error(), locked) {
							t.Errorf("error does not name %q: %v", locked, err)
						}
					}
					return
				}
			}
		case err := <-w.Errors():
			errs = append(errs, err)
		case <-deadline:
			t.Fatalf("no batch with app.go created within 2s; errors: %v", errs)
		}
	}
}

func TestWatcherStartsWithUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	w, err := New(root, Options{Debounce: 25 * time.Millisecond})
	if err != nil {
		t.Fatalf("new watcher with an unreadable subdirectory: %v", err)
	}
	defer w.Close()
}

// A directory that becomes unreadable keeps its previous entries rather than
// reporting them deleted, and its contents are seen again once it recovers.
func TestWatcherKeepsEntriesOfDirectoryThatBecomesUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	writeFile(t, filepath.Join(locked, "inside.go"), []byte("package inside\n"))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	w, err := New(root, Options{Debounce: 25 * time.Millisecond, PeriodicResync: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "app.go"), []byte("package app\n"))

	batch := waitForBatchAllowingErrors(t, w, 2*time.Second)
	assertChange(t, batch, "app.go", Created)
	assertNoChange(t, batch, "locked/inside.go")
	if _, ok := batch.Snapshot["locked/inside.go"]; !ok {
		t.Fatalf("snapshot dropped the unreadable directory's entry: %#v", batch.Snapshot)
	}

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(locked, "inside.go"), []byte("package inside // changed\n"))
	batch = waitForBatchAllowingErrors(t, w, 2*time.Second)
	assertChange(t, batch, "locked/inside.go", Modified)
}

// An unreadable directory is reported once while it stays unreadable, not on
// every resync, and again after it recovers and fails once more.
func TestWatcherReportsUnreadableDirectoryOncePerFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	w, err := New(root, Options{Debounce: 25 * time.Millisecond, PeriodicResync: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if got := collectErrors(w, 300*time.Millisecond); len(got) != 1 || !strings.Contains(got[0].Error(), locked) {
		t.Fatalf("want one error naming %q over many resyncs, got %v", locked, got)
	}

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := collectErrors(w, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("readable directory still reported: %v", got)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if got := collectErrors(w, 100*time.Millisecond); len(got) != 1 || !strings.Contains(got[0].Error(), locked) {
		t.Fatalf("want the directory reported again after it fails again, got %v", got)
	}
}

// Directories walked after an unreadable one are still watched natively: a
// change under one arrives without any periodic resync.
func TestWatcherWatchesDirectoriesAfterUnreadableOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "a-locked") // walked before "b"
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Mkdir(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := newTestWatcher(t, root)
	defer w.Close()

	writeFile(t, filepath.Join(root, "b", "x.go"), []byte("package b\n"))
	batch := waitForBatchAllowingErrors(t, w, 2*time.Second)
	assertChange(t, batch, "b/x.go", Created)
}

func waitForBatchAllowingErrors(t *testing.T, w *Watcher, timeout time.Duration) Batch {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case batch, ok := <-w.Batches():
			if !ok {
				t.Fatal("batches channel closed")
			}
			if len(batch.Changes) > 0 {
				return batch
			}
		case <-w.Errors():
		case <-deadline:
			t.Fatal("timed out waiting for batch")
		}
	}
}

// collectErrors drains Batches and Errors for d and returns the errors seen.
func collectErrors(w *Watcher, d time.Duration) []error {
	var errs []error
	deadline := time.After(d)
	for {
		select {
		case <-w.Batches():
		case err := <-w.Errors():
			errs = append(errs, err)
		case <-deadline:
			return errs
		}
	}
}

// An error dropped because Errors was full is not counted as reported: it is
// sent on a later pass, and each path is still reported only once.
func TestWatcherResendsUnreadableErrorDroppedFromFullBuffer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 directory")
	}
	root := t.TempDir()
	var locked []string
	for _, name := range []string{"a", "b", "c"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		locked = append(locked, dir)
	}

	// One slot: New can queue only the first of the three errors.
	w, err := New(root, Options{Debounce: 25 * time.Millisecond, PeriodicResync: 10 * time.Millisecond, ErrorBuffer: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	got := collectErrors(w, 300*time.Millisecond)
	if len(got) != len(locked) {
		t.Fatalf("want one error per unreadable directory, got %v", got)
	}
	for _, dir := range locked {
		n := 0
		for _, err := range got {
			if strings.Contains(err.Error(), dir+":") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q reported %d times, want once: %v", dir, n, got)
		}
	}
}
