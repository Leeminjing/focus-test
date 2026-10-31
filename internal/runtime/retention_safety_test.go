package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"focus-test-runtime/internal/evidence"
	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/snapshot"
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

func TestFullJournalCanSettleReleaseCollectAndResume(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "snapshots")
	store, err := snapshot.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Snapshots: store, StateDir: t.TempDir(), Retention: RetentionPolicy{MaxAge: time.Nanosecond, MinimumAge: time.Nanosecond, Interval: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j := &job{ID: "full", Legacy: true, Created: time.Now(), References: map[string]bool{"acceptance": true}, Spec: protocol.ExecutionRequest{Plan: protocol.Plan{Adapter: "fixture-v1"}},
		Result: protocol.ExecutionResult{Mode: protocol.Simulation, Binding: protocol.Binding{JobID: "full"}, Status: "running", CleanupStatus: "confirmed", Outcome: "inconclusive"}}
	message := strings.Repeat("x", 8900)
	for i := 1; i <= 7350; i++ {
		j.Events = append(j.Events, protocol.Event{JobID: j.ID, Seq: i, Kind: "case_finished", Message: message})
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	entries, err := s.checkpoint(nil)
	if err == nil {
		err = s.journal.Rewrite(entries)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if s.journal.Size() <= evidence.MaxJournal-(2<<20) {
		t.Fatal("fixture did not reach ordinary journal capacity")
	}
	// Recoverable collector failure must not prevent a finished execution from
	// publishing cleanup/terminal state using its reserved journal space.
	store.Close()
	if _, err := s.Collect(); err == nil {
		t.Fatal("collector fault was not observed")
	}
	if s.RetentionStatus()["last_collection_error"] == "" {
		t.Fatal("maintenance failure was hidden")
	}
	s.mu.Lock()
	err = s.commit(change{Kind: "finished", JobID: j.ID, ExecutionStatus: "exited"})
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("sweep blocked settlement: %v", err)
	}
	result, err := s.JobResult(j.ID)
	if err != nil || result.Status != "completed" {
		t.Fatalf("stranded execution: %+v %v", result, err)
	}
	reopened, err := snapshot.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.mu.Lock()
	s.config.Snapshots = reopened
	s.mu.Unlock()
	if err := s.Reference(j.ID, "acceptance", false); err != nil {
		t.Fatalf("full journal prevented reclaiming reference release: %v", err)
	}
	collected, err := s.Collect()
	if err != nil || collected.Jobs != 1 {
		t.Fatalf("capacity not recovered: %+v %v", collected, err)
	}
	if _, err := s.Submit(protocol.FixtureRequest{ProtocolVersion: 1, IdempotencyKey: "resumed", TestIDs: []string{"fixture.pass"}}); err != nil {
		t.Fatal("new admission did not resume", err)
	}
}

func TestPreparingWorkspaceIsReservedBeforeCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshots")
	store, err := snapshot.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input"), []byte("sealed"), 0600); err != nil {
		t.Fatal(err)
	}
	sealed, err := store.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Snapshots: store, StateDir: t.TempDir(), Retention: RetentionPolicy{InputGrace: time.Nanosecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gate, err := os.OpenFile(filepath.Join(root, "operations.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.LockOperation(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	release := func() {
		if gate != nil {
			evidence.UnlockOperation(gate)
			gate.Close()
			gate = nil
		}
	}
	defer release()
	j := &job{ID: "preparing", Created: time.Now(), Spec: protocol.ExecutionRequest{SnapshotID: sealed.SnapshotID, Plan: protocol.Plan{Adapter: "fixture-v1", TestIDs: []string{"fixture.pass"}}},
		Result: protocol.ExecutionResult{Mode: protocol.Simulation, Binding: protocol.Binding{JobID: "preparing", AttemptID: "attempt-PREPARING"}, Status: "running"}}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	done := make(chan struct{})
	s.wg.Go(func() { s.run(s.ctx, j); close(done) })
	deadline := time.Now().Add(3 * time.Second)
	reserved := ""
	for time.Now().Before(deadline) {
		r, _ := s.JobResult(j.ID)
		if r.Workspace != "" {
			reserved = r.Workspace
			break
		}
		time.Sleep(time.Millisecond)
	}
	if reserved == "" {
		release()
		t.Fatal("workspace was not published before copying")
	}
	collectionDone := make(chan error, 1)
	go func() { _, err := s.Collect(); collectionDone <- err }()
	release()
	if err := <-collectionDone; err != nil {
		t.Fatal(err)
	}
	<-done
	if err := store.VerifyWorkspace(context.Background(), sealed.SnapshotID, reserved); err != nil {
		t.Fatal("collector deleted preparing execution input", err)
	}
}
