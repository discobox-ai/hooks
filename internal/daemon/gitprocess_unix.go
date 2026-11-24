//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func configureGitCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gitTerminateGrace
}
