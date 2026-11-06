//go:build windows

package tools

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processGroup carries the job object that owns a command's process tree.
//
// Windows has no process groups in the Unix sense, so a shell's children do not
// die with the shell. A job object is the platform's answer: the shell is
// assigned to one, and terminating the job terminates everything it started,
// including grandchildren.
type processGroup struct {
	mu  sync.Mutex
	job windows.Handle
}

var processGroups sync.Map // *exec.Cmd -> *processGroup

// setProcessGroup puts the command in its own job object.
func setProcessGroup(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	// Close kills the tree, so a process that exits without being terminated
	// cannot leak its children either.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	processGroups.Store(cmd, &processGroup{job: job})
	return nil
}

// afterStart assigns the started process to the job.
//
// The assignment happens just after the shell starts, because a job can only
// receive a process that exists. A shell spends that moment before it reads the
// command, so its children are inside the job by the time there are any; a
// process created in that window would escape it, which is why the kill also
// terminates the shell itself.
func afterStart(cmd *exec.Cmd) error {
	v, ok := processGroups.Load(cmd)
	if !ok {
		return nil
	}
	g := v.(*processGroup)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 || cmd.Process == nil {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(g.job, h)
}

// killProcessGroup terminates the job, and with it every process the shell
// started.
func killProcessGroup(cmd *exec.Cmd) {
	v, ok := processGroups.LoadAndDelete(cmd)
	if ok {
		g := v.(*processGroup)
		g.mu.Lock()
		if g.job != 0 {
			_ = windows.TerminateJobObject(g.job, 1)
			windows.CloseHandle(g.job)
			g.job = 0
		}
		g.mu.Unlock()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// releaseProcessGroup drops the job once the command has finished, so a
// completed command does not keep a handle open (which would also fail a test's
// directory cleanup).
func releaseProcessGroup(cmd *exec.Cmd) {
	v, ok := processGroups.LoadAndDelete(cmd)
	if !ok {
		return
	}
	g := v.(*processGroup)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		windows.CloseHandle(g.job)
		g.job = 0
	}
}
