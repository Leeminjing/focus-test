package runner

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var finalDirectoryPath = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// EvalSymlinks does not resolve Windows mount-point junctions. Use the opened
// directory's final DOS path so Node's realpath and its permission grants agree.
func physicalDirectory(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("execution path is not a directory")
	}
	buffer := make([]uint16, 32768)
	n, _, callErr := finalDirectoryPath.Call(file.Fd(), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0)
	if n == 0 {
		return "", callErr
	}
	if n >= uintptr(len(buffer)) {
		return "", errors.New("final directory path exceeds Windows path limit")
	}
	final := syscall.UTF16ToString(buffer[:n])
	if strings.HasPrefix(final, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(final, `\\?\UNC\`), nil
	}
	return strings.TrimPrefix(final, `\\?\`), nil
}
