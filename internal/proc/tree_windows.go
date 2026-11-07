package proc

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree は Windows ではジョブオブジェクトでプロセスツリーをまとめる。
//
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE を付けているので、yard が異常終了して
// ハンドルが閉じられた場合も、ツールとその子プロセスは OS が終了させる。
//
// Windows にはコンソールを持たないプロセスへ穏当に終了を伝える汎用の手段が無い
// （サービスにはコンソールが無く、GenerateConsoleCtrlEvent が使えない）ため、
// Interrupt は失敗を返し、呼び出し側は即座に Kill する。
type processTree struct {
	cmd *exec.Cmd
	job windows.Handle
}

func startTree(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// ツールにコンソールを作らせない（出力はファイルか捨てるかのどちらか）。
		// 別グループにして、yard を Ctrl+C したときにツールへ直接届かないようにする。
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	// 起動直後からジョブに入れるまでの間に作られた孫プロセスは漏れうる。
	// CREATE_SUSPENDED で起動すれば防げるが、os/exec からはメインスレッドを
	// 再開する手段が無いため受け入れている。
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err != nil {
		// ジョブに入れられなくても、本体だけは止められるので動かし続ける。
		windows.CloseHandle(job)
		return &processTree{cmd: cmd}, nil
	}
	return &processTree{cmd: cmd, job: job}, nil
}

func (t *processTree) Interrupt() error { return windows.ERROR_NOT_SUPPORTED }

func (t *processTree) Kill() error {
	if t.job != 0 {
		return windows.TerminateJobObject(t.job, 1)
	}
	return t.cmd.Process.Kill()
}

func (t *processTree) Close() {
	if t.job != 0 {
		// KILL_ON_JOB_CLOSE により、残っている孫プロセスもここで終わる。
		windows.CloseHandle(t.job)
		t.job = 0
	}
}
