// Package runner executes supported local adapters in managed process trees.
// Process containment is not a filesystem or network sandbox.
package runner

import (
	"context"
	"os"
)

type processResult struct {
	ExitCode int
	Cleanup  string
	Err      error
}

type command struct {
	Executable  string
	Args        []string
	Directory   string
	Environment []string
	Stdout      *os.File
	Stderr      *os.File
}

type process interface {
	Wait(context.Context) processResult
}
