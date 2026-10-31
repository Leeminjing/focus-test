package evidence

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

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

func tryLock(f *os.File) error {
	overlapped := new(syscall.Overlapped)
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	result, _, callErr := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	runtime.KeepAlive(overlapped)
	if result == 0 {
		if callErr == syscall.Errno(33) {
			return ErrLockBusy
		}
		return callErr
	}
	return nil
}

func UnlockOperation(f *os.File) error {
	overlapped := new(syscall.Overlapped)
	result, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx").Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	runtime.KeepAlive(overlapped)
	if result == 0 {
		return err
	}
	return nil
}
