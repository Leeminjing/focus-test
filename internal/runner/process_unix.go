//go:build !windows

package runner

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type unixProcess struct{ command *exec.Cmd }

func start(c command) (process, error) {
	if err := enableReaping(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Executable, c.Args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = c.Directory, c.Environment, c.Stdout, c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &unixProcess{command: cmd}, nil
}

func (p *unixProcess) Wait(ctx context.Context) processResult {
	done := make(chan error, 1)
	go func() { done <- p.command.Wait() }()
	r := processResult{ExitCode: -1, Cleanup: "unknown"}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			r.Err = err
		}
	case <-ctx.Done():
		r.Err = ctx.Err()
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		<-done
	}
	r.ExitCode = p.command.ProcessState.ExitCode()
	_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reapGroup(p.command.Process.Pid)
		if errors.Is(syscall.Kill(-p.command.Process.Pid, 0), syscall.ESRCH) {
			r.Cleanup = "confirmed"
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Cleanup != "confirmed" {
		r.Err = errors.Join(r.Err, errors.New("process group cleanup unconfirmed"))
	}
	return r
}
