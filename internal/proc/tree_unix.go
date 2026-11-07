//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// processTree は Windows 以外ではプロセスグループでプロセスツリーをまとめる。
// ツールを新しいグループのリーダーとして起動し、グループ全体にシグナルを送る。
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

// Close は本体の終了後に、グループに残った子プロセスを片付ける。
func (t *processTree) Close() {
	syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}
