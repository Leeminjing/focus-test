//go:build !windows

package evidence

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLockBusy
	}
	return err
}

func UnlockOperation(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("runtime state is already locked: %w", err)
	}
	return f, nil
}
