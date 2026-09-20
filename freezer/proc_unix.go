//go:build !windows

package freezer

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child into its own process group so a timeout can kill
// everything qm started.
func setProcGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
