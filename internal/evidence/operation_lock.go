package evidence

import (
	"context"
	"errors"
	"os"
	"time"
)

var ErrLockBusy = errors.New("OS lock is busy")

// LockOperation locks an already-rooted file. Closing that file releases the
// lock, including on crash. Snapshot writers and collectors share this seam.
func LockOperation(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := tryLock(f)
		if !errors.Is(err, ErrLockBusy) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
