package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/hooks/models"
)

// Exercise the daemon's run/check state in a disposable copy of the test binary.
// The language server uses the intact test binary so only helper re-exec loses
// its pathname.
func TestLSPRunsAfterDaemonExecutableUnlinked(t *testing.T) {
	serverExe := os.Getenv("HOOKS_TEST_INTACT_SERVER")
	if serverExe == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		copied := filepath.Join(t.TempDir(), "daemon.test")
		if err := os.WriteFile(copied, data, 0755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, copied, "-test.run=^TestLSPRunsAfterDaemonExecutableUnlinked$")
		cmd.Env = append(os.Environ(), "HOOKS_TEST_INTACT_SERVER="+exe)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("daemon regression: %v\n%s", err, out)
		}
		return
	}
	repo := initGitTestRepo(t)
	hookDir := filepath.Join(repo, ".discobox", "hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
#---
# name: Go LSP
# type: file
# engine: lsp
# pattern: "**/*.go"
# language_id: go
#---
HOOKS_TEST_LSP_SERVER=1 exec '%s' -test.run '^TestLSPStartupServer$'
`, strings.ReplaceAll(serverExe, "'", "'\\''"))
	if err := os.WriteFile(filepath.Join(hookDir, "go-lsp.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	r, err := newRuntime(context.Background(), Config{SessionID: "unlink", RepoRoot: repo, DBPath: filepath.Join(state, "hooks.db"), SocketPath: filepath.Join(state, "hooks.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.store.Close()
	defer r.cancel()
	defer r.closeLSPClients()
	r.syncLSPHooks()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if attempt == 1 {
			closeLSPRuntime(r.lspClients["go-lsp"])
			// Replacing the deleted pathname must not switch the helper to a different
			// executable on the next launch.
			if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := r.requestLSPRun("go-lsp", false); err != nil || !ok {
			t.Fatalf("run %d: %t, %v", attempt, ok, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			check, err := r.waitSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if check.Settled && len(check.Hooks) == 1 &&
				(check.Hooks[0].Status == models.StatusSuccess || check.Hooks[0].Status == models.StatusFailure) {
				if check.Hooks[0].Status != models.StatusSuccess {
					t.Fatalf("check %d: %+v", attempt, check)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("check %d did not settle: %+v", attempt, check)
			}
			time.Sleep(time.Millisecond)
		}
	}
}
