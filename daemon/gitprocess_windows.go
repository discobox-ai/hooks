//go:build windows

package daemon

import (
	"os"
	"os/exec"
)

// Windows has no signal to deliver that git could clean up after, so the kill
// stays and only the wait is bounded.
func configureGitCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = gitTerminateGrace
}
