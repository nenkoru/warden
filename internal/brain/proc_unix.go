//go:build unix

package brain

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the agent in its own process group so killGroup also
// reaches whatever it spawned.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // already gone is fine
	}
}
