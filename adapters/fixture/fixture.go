// Package fixture supplies a deterministic in-process simulation. It does not
// launch commands, read project files, or prove application correctness.
package fixture

import (
	"context"
	"time"
)

// Run emits one case event at a time. A slow fixture makes early failure
// observable while a later case remains unfinished.
func Run(ctx context.Context, ids []string, emit func(kind, testID string)) error {
	for _, id := range ids {
		delay := 10 * time.Millisecond
		if id == "fixture.slow" {
			delay = 500 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		kind := "case_finished"
		if id == "fixture.fail" {
			kind = "case_failed"
		}
		emit(kind, id)
	}
	return nil
}
