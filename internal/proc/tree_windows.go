package proc

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree groups the process tree with a job object on Windows.
//
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE is set, so even if yard dies abnormally and
// the handle is closed, the OS terminates the server and its child processes.
//
// Windows has no general way to ask a process without a console to exit gracefully
// (a service has no console, so GenerateConsoleCtrlEvent cannot be used), so
// Interrupt returns an error and the caller kills immediately.
type processTree struct {
	cmd *exec.Cmd
	job windows.Handle
}

func startTree(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Do not give the server a console (output goes to a file or is discarded).
		// Use a separate group so that Ctrl+C on yard is not delivered to the server directly.
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

	// Grandchild processes created between start and being assigned to the job can leak.
	// Starting with CREATE_SUSPENDED would prevent this, but os/exec offers no way to
	// resume the main thread, so this is accepted.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err != nil {
		// Even without the job, the main process can still be stopped, so keep it running.
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
		// With KILL_ON_JOB_CLOSE, remaining grandchild processes also end here.
		windows.CloseHandle(t.job)
		t.job = 0
	}
}
