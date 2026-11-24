package daemon

import (
	"context"
	"os/exec"
	"time"
)

// gitTerminateGrace bounds how long a cancelled git may take to run its own
// lockfile cleanup before os/exec resorts to the hard kill.
const gitTerminateGrace = 2 * time.Second

// gitCommand builds a git invocation for the daemon's own bookkeeping.
//
// The daemon reads git state on background timers, against a repository someone
// is actively working in, so two rules apply to every call it makes:
//
//   - --no-optional-locks stops git from taking .git/index.lock just to write
//     back an index it refreshed on the way past. `git status` does that by
//     default, which both races the user's own git commands and strands the
//     lock file when the refreshing git dies mid-write. Locks git genuinely
//     needs, such as the one snapshot capture's `git add` takes on
//     GIT_INDEX_FILE, are unaffected.
//   - Cancellation terminates rather than SIGKILLing, so git's lockfile signal
//     handlers get to remove whatever it did lock. os/exec's default for a
//     context-bound command is Process.Kill, which git cannot clean up after.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	configureGitCancel(cmd)
	return cmd
}
