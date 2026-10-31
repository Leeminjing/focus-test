package runtime

import (
	"testing"
	"time"

	"focus-test-runtime/internal/protocol"
)

func TestCollectionNeverDropsUnknownCleanup(t *testing.T) {
	s := New()
	defer s.Close()
	s.mu.Lock()
	j := &job{ID: "uncertain", Legacy: true, Created: time.Now().Add(-72 * time.Hour), Finished: time.Now().Add(-48 * time.Hour),
		Result: protocol.ExecutionResult{Binding: protocol.Binding{JobID: "uncertain"}, Status: "completed", CleanupStatus: "unknown", Outcome: "inconclusive"}}
	s.jobs[j.ID] = j
	s.mu.Unlock()
	result, err := s.Collect()
	if err != nil || result.Jobs != 0 {
		t.Fatalf("unknown cleanup evicted: %+v %v", result, err)
	}
	if result, err := s.JobResult(j.ID); err != nil || result.CleanupStatus != "unknown" {
		t.Fatalf("lost quarantine evidence: %+v %v", result, err)
	}
}
