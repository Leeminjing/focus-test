package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/snapshot"
)

func executionFixture(t *testing.T, cpu int) (*runtime.Service, runtime.Config, protocol.ExecutionRequest) {
	t.Helper()
	store, err := snapshot.Open(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "test.txt"), []byte("fixed input"), 0600); err != nil {
		t.Fatal(err)
	}
	sealed, err := store.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{Snapshots: store, StateDir: filepath.Join(t.TempDir(), "state"), CPUUnits: cpu, MemoryMiB: 512}
	s, err := runtime.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	r := protocol.ExecutionRequest{ProtocolVersion: 1, IdempotencyKey: "first", CallerRef: "A", TrustDomain: "test-project",
		SnapshotID: sealed.SnapshotID, Plan: protocol.Plan{Adapter: "fixture-v1", TestIDs: []string{"fixture.fail", "fixture.slow"}, Workers: 1},
		Environment: runtime.FixtureEnvironment(), Policy: protocol.Policy{ExecutionTimeoutMS: 3000, Isolation: "local-process", Network: "declared"},
		Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 5000, ReusePolicy: "inflight"}
	return s, config, r
}

func waitResult(t *testing.T, s *runtime.Service, id string) protocol.ExecutionResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.ExecutionResult(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == "completed" || r.Status == "cancelled" || r.Status == "expired" {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("request did not settle")
	return protocol.ExecutionResult{}
}

func TestSharedExecutionAndIndependentCancellation(t *testing.T) {
	s, _, r := executionFixture(t, 2)
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	r.CallerRef, r.IdempotencyKey = "B", "second"
	b, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestID == b.RequestID || a.JobID != b.JobID {
		t.Fatalf("wrong sharing: %+v %+v", a, b)
	}
	if err := s.Cancel(a.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(a.RequestID); err != nil {
		t.Fatal(err)
	}
	if result, _ := s.ExecutionResult(a.RequestID); result.Status != "cancelled" || result.Outcome != "inconclusive" {
		t.Fatalf("cancel ignored: %+v", result)
	}
	result := waitResult(t, s, b.RequestID)
	if result.Outcome != "failed" || result.Completed != 2 || result.AttemptID == "" || result.Mode != protocol.Simulation {
		t.Fatalf("subscriber lost execution: %+v", result)
	}
	page, err := s.Events(b.JobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, event := range page.Events {
		if event.Kind == "attempt_started" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("shared work started %d attempts", starts)
	}
}

func TestManyContextsMergeAtomically(t *testing.T) {
	s, _, r := executionFixture(t, 2)
	var wg sync.WaitGroup
	ids := make(chan protocol.Binding, 16)
	for i := range 16 {
		wg.Go(func() {
			owned := r
			owned.CallerRef = fmt.Sprint(i)
			bound, err := s.SubmitExecution(owned)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- bound
		})
	}
	wg.Wait()
	close(ids)
	jobs, requests := map[string]bool{}, map[string]bool{}
	for bound := range ids {
		jobs[bound.JobID] = true
		requests[bound.RequestID] = true
	}
	if len(jobs) != 1 || len(requests) != 16 {
		t.Fatalf("atomic merge failed: %d jobs, %d requests", len(jobs), len(requests))
	}
}

func TestBudgetExclusiveResourcesAndTimeouts(t *testing.T) {
	s, _, r := executionFixture(t, 2)
	r.Plan.TestIDs = []string{"fixture.slow"}
	r.ReusePolicy = "none"
	r.Resources.Exclusive = []string{"database"}
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	r.IdempotencyKey = "queued"
	r.CallerRef = "B"
	b, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if result, _ := s.ExecutionResult(b.RequestID); result.Status != "queued" {
		t.Fatalf("exclusive overlap: %+v", result)
	}
	r.IdempotencyKey = "too-large"
	r.Resources.CPUUnits = 3
	if _, err := s.SubmitExecution(r); !errors.Is(err, runtime.ErrResources) {
		t.Fatalf("impossible budget accepted: %v", err)
	}
	if err := s.Cancel(a.RequestID); err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, s, b.RequestID)
	if result.Outcome != "passed" {
		t.Fatalf("resource not released: %+v", result)
	}
	r.Resources.CPUUnits = 1
	r.IdempotencyKey = "timeout"
	r.Policy.ExecutionTimeoutMS = 10
	c, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	result = waitResult(t, s, c.RequestID)
	if result.ExecutionStatus != "timed_out" || result.Outcome != "inconclusive" {
		t.Fatalf("timeout became pass: %+v", result)
	}
}

func TestRequestExpiryDoesNotCancelOtherSubscriber(t *testing.T) {
	s, _, r := executionFixture(t, 1)
	r.Plan.TestIDs = []string{"fixture.slow"}
	r.RequestTimeoutMS = 20
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	r.RequestTimeoutMS = 3000
	r.CallerRef = "B"
	b, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.JobID != b.JobID {
		t.Fatal("waiting deadline entered execution key")
	}
	if result := waitResult(t, s, a.RequestID); result.Status != "expired" {
		t.Fatalf("request did not expire: %+v", result)
	}
	if result := waitResult(t, s, b.RequestID); result.Outcome != "passed" {
		t.Fatalf("expiry killed shared execution: %+v", result)
	}
}

func TestDurableRetryAndResultsSurviveRestart(t *testing.T) {
	s, config, r := executionFixture(t, 1)
	r.Plan.TestIDs = []string{"fixture.pass"}
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	result := waitResult(t, s, a.RequestID)
	s.Close()
	restarted, err := runtime.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	b, err := restarted.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestID != b.RequestID || a.JobID != b.JobID {
		t.Fatal("retry after restart created new execution")
	}
	actual, err := restarted.ExecutionResult(b.RequestID)
	if err != nil || actual.Outcome != result.Outcome || actual.AttemptID != result.AttemptID {
		t.Fatalf("durable evidence changed: %+v %v", actual, err)
	}
	r.CallerRef = "new-context"
	c, err := restarted.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.JobID == a.JobID {
		t.Fatal("inflight policy incorrectly reused completed result")
	}
}

func TestActiveStateRecoversAsInterruptedWithoutReplay(t *testing.T) {
	s, config, r := executionFixture(t, 1)
	r.Plan.TestIDs = []string{"fixture.slow"}
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	// A copied durable prefix models an unclean daemon exit without terminating
	// a test process or relying on PID timing.
	data, err := os.ReadFile(filepath.Join(config.StateDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recoveredConfig := config
	recoveredConfig.StateDir = filepath.Join(t.TempDir(), "recovery")
	if err := os.MkdirAll(recoveredConfig.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recoveredConfig.StateDir, "events.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := runtime.Open(recoveredConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	result, err := recovered.ExecutionResult(a.RequestID)
	if err != nil || result.ExecutionStatus != "interrupted" || result.Outcome != "inconclusive" {
		t.Fatalf("unfinished work was replayed or passed: %+v %v", result, err)
	}
}
