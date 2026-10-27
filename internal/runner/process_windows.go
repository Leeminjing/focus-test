package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var createJob = kernel.NewProc("CreateJobObjectW")
var setJob = kernel.NewProc("SetInformationJobObject")
var assignJob = kernel.NewProc("AssignProcessToJobObject")
var terminateJob = kernel.NewProc("TerminateJobObject")
var queryJob = kernel.NewProc("QueryInformationJobObject")
var resumeThread = kernel.NewProc("ResumeThread")
var createMu sync.Mutex

type basicLimits struct {
	ProcessTime     int64
	JobTime         int64
	Flags           uint32
	MinWorkingSet   uintptr
	MaxWorkingSet   uintptr
	ActiveProcesses uint32
	Affinity        uintptr
	Priority        uint32
	Scheduling      uint32
}
type extendedLimits struct {
	Basic             basicLimits
	IO                [6]uint64
	ProcessMemory     uintptr
	JobMemory         uintptr
	PeakProcessMemory uintptr
	PeakJobMemory     uintptr
}
type accounting struct {
	Times      [4]int64
	Faults     uint32
	Total      uint32
	Active     uint32
	Terminated uint32
}
type windowsProcess struct {
	process syscall.Handle
	job     syscall.Handle
}

func start(c command) (process, error) {
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	limits := extendedLimits{Basic: basicLimits{Flags: 0x2000}} // KILL_ON_JOB_CLOSE; no breakaway.
	ok, _, err := setJob.Call(job, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if ok == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	application, err := syscall.UTF16PtrFromString(c.Executable)
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	args := []string{syscall.EscapeArg(c.Executable)}
	for _, arg := range c.Args {
		args = append(args, syscall.EscapeArg(arg))
	}
	line, err := syscall.UTF16FromString(strings.Join(args, " "))
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	directory, err := syscall.UTF16PtrFromString(c.Directory)
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	env := append([]string(nil), c.Environment...)
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	block := []uint16{}
	for _, entry := range env {
		wide, e := syscall.UTF16FromString(entry)
		if e != nil {
			syscall.CloseHandle(syscall.Handle(job))
			return nil, e
		}
		block = append(block, wide...)
	}
	block = append(block, 0)
	if len(block) == 1 {
		block = append(block, 0)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	defer input.Close()
	handles := []syscall.Handle{syscall.Handle(input.Fd()), syscall.Handle(c.Stdout.Fd()), syscall.Handle(c.Stderr.Fd())}
	// Serialize the temporary inheritable handles. Test code starts only after
	// successful job assignment, so even immediate child spawning is contained.
	createMu.Lock()
	defer createMu.Unlock()
	for _, h := range handles {
		if err := syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
			syscall.CloseHandle(syscall.Handle(job))
			return nil, err
		}
		defer syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, 0)
	}
	si := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{})), Flags: syscall.STARTF_USESTDHANDLES, StdInput: handles[0], StdOutput: handles[1], StdErr: handles[2]}
	var pi syscall.ProcessInformation
	err = syscall.CreateProcess(application, &line[0], nil, nil, true, 0x00000004|0x08000000|0x00000400, &block[0], directory, &si, &pi)
	runtime.KeepAlive(block)
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	defer syscall.CloseHandle(pi.Thread)
	ok, _, err = assignJob.Call(job, uintptr(pi.Process))
	if ok == 0 {
		syscall.TerminateProcess(pi.Process, 1)
		syscall.WaitForSingleObject(pi.Process, 3000)
		syscall.CloseHandle(pi.Process)
		syscall.CloseHandle(syscall.Handle(job))
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	resumed, _, err := resumeThread.Call(uintptr(pi.Thread))
	if uint32(resumed) == 0xffffffff {
		terminateJob.Call(job, 1)
		syscall.WaitForSingleObject(pi.Process, 3000)
		syscall.CloseHandle(pi.Process)
		syscall.CloseHandle(syscall.Handle(job))
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	return &windowsProcess{process: pi.Process, job: syscall.Handle(job)}, nil
}

func (p *windowsProcess) Wait(ctx context.Context) processResult {
	defer syscall.CloseHandle(p.process)
	defer syscall.CloseHandle(p.job)
	result := processResult{ExitCode: -1, Cleanup: "unknown"}
	for {
		state, err := syscall.WaitForSingleObject(p.process, 10)
		if err != nil {
			result.Err = err
			break
		}
		if state == syscall.WAIT_OBJECT_0 {
			break
		}
		if err := ctx.Err(); err != nil {
			result.Err = err
			break
		}
	}
	// The primary process exiting does not prove descendants exited. Always
	// terminate the remainder and query the job until its active count is zero.
	if ok, _, err := terminateJob.Call(uintptr(p.job), 1); ok == 0 {
		result.Err = errors.Join(result.Err, fmt.Errorf("TerminateJobObject: %w", err))
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var info accounting
		ok, _, err := queryJob.Call(uintptr(p.job), 1, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		if ok == 0 {
			result.Err = errors.Join(result.Err, err)
			break
		}
		if info.Active == 0 {
			result.Cleanup = "confirmed"
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Cleanup != "confirmed" {
		result.Err = errors.Join(result.Err, errors.New("process tree cleanup unconfirmed"))
	}
	var exit uint32
	if err := syscall.GetExitCodeProcess(p.process, &exit); err != nil {
		result.Err = errors.Join(result.Err, err)
	} else {
		result.ExitCode = int(exit)
	}
	return result
}
