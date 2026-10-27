package runner

import "syscall"

func enableReaping() error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0) // PR_SET_CHILD_SUBREAPER
	if errno != 0 {
		return errno
	}
	return nil
}
func reapGroup(group int) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-group, &status, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return
		}
	}
}
