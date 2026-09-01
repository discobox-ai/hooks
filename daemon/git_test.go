package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDaemonGitCallsDoNotTouchTheRepositoryIndex pins the reason the daemon
// passes --no-optional-locks: `git status` otherwise writes a refreshed index
// back through .git/index.lock, which races the user's own git commands and
// leaves the lock behind when the daemon's git is killed mid-write.
func TestDaemonGitCallsDoNotTouchTheRepositoryIndex(t *testing.T) {
	ctx := context.Background()
	repo := initGitTestRepo(t)

	tracked := filepath.Join(repo, "tracked.txt")
	// A stat change with identical content is what makes git want to rewrite
	// the index: the cached stat data no longer matches the working file. The
	// timestamp is well in the past so git does not treat the entry as racily
	// clean and skip the refresh this test depends on.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(tracked, past, past); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	if changes := initialWorkingTreeChanges(ctx, repo); len(changes) != 0 {
		t.Fatalf("expected a clean worktree, got %v", changes)
	}
	if _, err := workspaceSnapshotCandidates(ctx, repo); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("daemon git calls rewrote .git/index; they must not take the repository index lock")
	}
	if _, err := os.Stat(indexPath + ".lock"); err == nil {
		t.Fatal("daemon git calls left .git/index.lock behind")
	}
}

func initGitTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitForTest(t, repo, "init")
	runGitForTest(t, repo, "config", "user.email", "test@example.com")
	runGitForTest(t, repo, "config", "user.name", "Test User")
	runGitForTest(t, repo, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, repo, "add", "tracked.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	// Settle the index so the assertions below observe only what the daemon did.
	runGitForTest(t, repo, "status", "--porcelain")
	return repo
}

func runGitForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}
