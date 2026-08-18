//go:build windows

package host

import (
	"fmt"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows has no process groups in the POSIX sense, so THE GROUP IS A JOB.
//
// The invariant this file exists to preserve is the one stated in host.go: the
// unit is the whole tree, not the leader. On unix that is `kill(-pgid)`. Here
// the child is placed in a Job Object, every process it spawns is in that job
// automatically, and terminating the job takes the tree down together.
//
// Killing only the leader would repeat the bug the unix path was written to
// avoid: `cmd /C` exits as soon as it has started llama-server, so a "backend
// exited" log line would coexist with a live process still holding the GPU.
//
// UNVERIFIED. This has never run on Windows — there is no Windows machine here
// to run it on. It compiles and the semantics follow the documented API, which
// is not the same as being known to work. Treat the first deployment as a test,
// and watch for orphaned processes specifically.

// jobs maps a spawned process id to the job that owns its tree.
//
// A side table because the platform API is (pid) → error, which is all unix
// needs. The alternative is threading an opaque handle through Handle and every
// caller for the benefit of one platform.
var jobs struct {
	sync.Mutex
	m map[int]windows.Handle
}

// sysProcAttr starts the backend in a new process group.
//
// CREATE_NEW_PROCESS_GROUP is what makes a console control event addressable to
// this child and its descendants rather than to everything sharing our console
// — including this daemon. It is also required for GenerateConsoleCtrlEvent to
// target the child at all.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// adoptGroup puts a freshly started process into a job that owns its tree.
//
// There is a race here worth naming: the child is already running by the time
// it is assigned, so a grandchild spawned in that window escapes the job. Go's
// exec gives no way to start suspended (it does not expose the thread handle
// needed to resume), so the window cannot be closed without reimplementing
// CreateProcess. It is microseconds against a backend that takes seconds to
// initialise, but it is not zero.
func adoptGroup(pid int) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	// KILL_ON_JOB_CLOSE is the backstop: if this daemon dies without reaping,
	// the last handle to the job closes and Windows takes the tree with it.
	// That is strictly better than the unix behaviour, where an orphaned group
	// survives its parent.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafePointer(&info)), uint32(unsafeSizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("configure job object: %w", err)
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("assign process %d to job: %w", pid, err)
	}

	jobs.Lock()
	if jobs.m == nil {
		jobs.m = map[int]windows.Handle{}
	}
	jobs.m[pid] = job
	jobs.Unlock()
	return nil
}

// releaseGroup drops the job once the tree is gone.
//
// Called when Wait returns. Closing the handle also enforces KILL_ON_JOB_CLOSE,
// which is harmless on an already-dead tree and is the safety net on a live one.
func releaseGroup(pid int) {
	jobs.Lock()
	job, ok := jobs.m[pid]
	delete(jobs.m, pid)
	jobs.Unlock()
	if ok {
		windows.CloseHandle(job)
	}
}

// killGroup asks the tree to stop — the closest thing to SIGTERM.
//
// CTRL_BREAK rather than CTRL_C: only CTRL_BREAK can be sent to a specific
// process group, and CTRL_C cannot be delivered to a group created with
// CREATE_NEW_PROCESS_GROUP at all.
//
// It fails when the daemon has no console, which is exactly the case when it
// runs as a service. That is survivable rather than fatal: proc.Manager treats
// a stop as a REQUEST, waits out its grace period, and then calls
// killGroupHard, which does not depend on a console.
func killGroup(pid int) error {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
		return fmt.Errorf("ctrl-break to %d (no console when running as a service; the grace period will escalate to a hard kill): %w", pid, err)
	}
	return nil
}

// killGroupHard terminates the whole job. The Windows analogue of SIGKILL to a
// process group, and the reason a job is used at all.
func killGroupHard(pid int) error {
	jobs.Lock()
	job, ok := jobs.m[pid]
	jobs.Unlock()
	if !ok {
		// No job: either it was never adopted or it has already been released.
		// Fall back to the leader so something still happens.
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return fmt.Errorf("open process %d: %w", pid, err)
		}
		defer windows.CloseHandle(h)
		return windows.TerminateProcess(h, 1)
	}
	return windows.TerminateJobObject(job, 1)
}

// groupAlive reports whether ANY process remains in the tree.
//
// Asked of the JOB, not the leader, for the reason in host.go: the leader is a
// shell that exits as soon as it has started the real process, and reporting
// the tree dead while a backend still holds the GPU is the failure this whole
// abstraction exists to prevent.
func groupAlive(pid int) bool {
	jobs.Lock()
	job, ok := jobs.m[pid]
	jobs.Unlock()
	if !ok {
		return false
	}
	var info jobBasicAccounting
	if err := windows.QueryInformationJobObject(job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafePointer(&info)), uint32(unsafeSizeof(info)), nil); err != nil {
		// Cannot tell. Say ALIVE: a wrong "dead" frees a reservation a live
		// process is still holding, which is the more expensive mistake.
		return true
	}
	return info.ActiveProcesses > 0
}

// jobBasicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// x/sys/windows names a class constant for but does not declare a type for.
// Field order and widths are the documented layout; only ActiveProcesses is
// read, but the whole struct must be present or the query writes past it.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}
