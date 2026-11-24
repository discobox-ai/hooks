package processhelper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHelperStartsAfterExecutableUnlinked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux executable unlink semantics")
	}
	if os.Getenv("HOOKS_TEST_UNLINK_SELF") == "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(exe); err != nil {
			t.Fatal(err)
		}
		cmd, err := CommandContext(context.Background(), CommandOptions{Command: "sh", Args: []string{"-c", "exit 0"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("helper must launch after daemon executable disappears: %v", err)
		}
		_ = cmd.Wait()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "discobox-hooks.test")
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(copied, "-test.run=^TestHelperStartsAfterExecutableUnlinked$")
	cmd.Env = append(os.Environ(), "HOOKS_TEST_UNLINK_SELF=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper launch failed: %v\n%s", err, out)
	}
}
