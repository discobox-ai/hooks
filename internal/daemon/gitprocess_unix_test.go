//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestGitCommandCancelTerminates pins the second half of the index.lock fix:
// a cancelled daemon git must get a signal it can install a handler for, so
// git's own lockfile cleanup runs. The os/exec default is SIGKILL, which git
// cannot clean up after.
func TestGitCommandCancelTerminates(t *testing.T) {
	repo := initGitTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `cat-file --batch` blocks on stdin, so the command is still running when
	// the context is cancelled.
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdinWriter.Close()

	cmd := gitCommand(ctx, repo, "cat-file", "--batch")
	cmd.Stdin = stdin
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the cancelled git to exit on a signal, got %v", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("expected a signalled exit, got %v", exitErr)
	}
	if got := status.Signal(); got != syscall.SIGTERM {
		t.Fatalf("cancelled git got %v, want SIGTERM so it can clean up its lock files", got)
	}
}
