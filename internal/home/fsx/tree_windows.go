package fsx

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The processes started by StartTree/RunTree, with the job each was put in.
var jobs sync.Map // *exec.Cmd -> windows.Handle

// KillTree makes cancelling cmd's context kill the process and everything it started. Windows has no process groups; a process
// started by StartTree or RunTree is put in a Job Object (kill on close) before it runs a single instruction, so everything it
// starts is in the job too, and cancelling terminates the job. Cancelling then also runs the guarded walk over the process table
// (Reap), which catches what the job could not hold: everything, when the command was started by plain cmd.Start/Run (no job),
// or when the job could not be made or joined.
func KillTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		jobDone := false
		if v, ok := jobs.LoadAndDelete(cmd); ok {
			job := v.(windows.Handle)
			jobDone = windows.TerminateJobObject(job, 1) == nil
			windows.CloseHandle(job)
		}
		killDescendants(uint32(cmd.Process.Pid))
		if jobDone {
			return nil
		}
		return cmd.Process.Kill()
	}
}

// StartTree is cmd.Start with the process in a Job Object that is terminated when cmd is cancelled (KillTree), when the job is
// released (WaitTree, ReleaseTree) or when this process ends. The process is created suspended, put in the job, and only then
// resumed, so nothing it starts can be outside the job. If the job can't be made or joined (the system may refuse), the process
// still runs and cancelling falls back to the walk over the process table. Release the job with WaitTree or ReleaseTree.
func StartTree(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid)
	job, assigned := newJob(), false
	if job != 0 {
		assigned = assignToJob(job, pid)
	}
	if err := resumeProcess(pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if job != 0 {
			windows.CloseHandle(job)
		}
		return err
	}
	switch {
	case assigned:
		jobs.Store(cmd, job)
	case job != 0:
		windows.CloseHandle(job)
	}
	return nil
}

// WaitTree is cmd.Wait that also lets go of the job StartTree made (anything still in it is terminated with it).
func WaitTree(cmd *exec.Cmd) error {
	err := cmd.Wait()
	ReleaseTree(cmd)
	return err
}

// ReleaseTree lets go of the job StartTree made for cmd, terminating what is still in it. It is for a caller that does not use
// WaitTree; calling it again, or after a cancel, does nothing.
func ReleaseTree(cmd *exec.Cmd) {
	if v, ok := jobs.LoadAndDelete(cmd); ok {
		windows.CloseHandle(v.(windows.Handle))
	}
}

// RunTree is StartTree and WaitTree.
func RunTree(cmd *exec.Cmd) error {
	if err := StartTree(cmd); err != nil {
		return err
	}
	return WaitTree(cmd)
}

func newJob() windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE}}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0
	}
	return job
}

func assignToJob(job windows.Handle, pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(job, h) == nil
}

// resumeProcess resumes the threads of a process created suspended (a new process has the one).
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var e windows.ThreadEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	resumed := 0
	for err := windows.Thread32First(snap, &e); err == nil; err = windows.Thread32Next(snap, &e) {
		if e.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, e.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(th)
		windows.CloseHandle(th)
		if err != nil {
			return err
		}
		resumed++
	}
	if resumed == 0 {
		return errors.New("the new process has no thread to start")
	}
	return nil
}

// creationTime is a process's creation time as FILETIME ticks (0 when it can't be read).
func creationTime(h windows.Handle) int64 {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime)
}

// processTable is the list of processes with their creation times (0 where a process can't be opened).
func processTable() []Proc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var table []Proc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		p := Proc{PID: e.ProcessID, PPID: e.ParentProcessID}
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID); err == nil {
			p.Created = creationTime(h)
			windows.CloseHandle(h)
		}
		table = append(table, p)
	}
	return table
}

// killDescendants terminates every process that descends from pid, guarded against reused process ids (see Reap). pid is the
// process of a cmd that is not yet waited for, so its handle is open and the id can't have been given to anyone else.
func killDescendants(pid uint32) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return
	}
	rootCreated := creationTime(h)
	windows.CloseHandle(h)
	Reap(processTable, pid, rootCreated, 4, func(p uint32) (int64, func(bool), bool) {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p)
		if err != nil {
			return 0, nil, false
		}
		return creationTime(h), func(terminate bool) {
			if terminate {
				_ = windows.TerminateProcess(h, 1)
			}
			windows.CloseHandle(h)
		}, true
	})
}
