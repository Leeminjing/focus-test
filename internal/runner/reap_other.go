//go:build !windows && !linux

package runner

func enableReaping() error { return nil }
func reapGroup(group int)  {}
