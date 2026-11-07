//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// processTree groups the process tree with a process group on non-Windows systems.
// The server is started as the leader of a new group, and signals go to the whole group.
type processTree struct {
	cmd *exec.Cmd
}

func startTree(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processTree{cmd: cmd}, nil
}

func (t *processTree) Interrupt() error {
	return syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
}

func (t *processTree) Kill() error {
	return syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}

// Close cleans up child processes left in the group after the main process exits.
func (t *processTree) Close() {
	syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}
