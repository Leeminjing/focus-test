//go:build !windows

package runner

import "path/filepath"

func physicalDirectory(name string) (string, error) { return filepath.EvalSymlinks(name) }
