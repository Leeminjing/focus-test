package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runner"
	"focus-test-runtime/internal/runtime"
)

func TestRealSharedExecutionCancellationAndSnapshotSeparation(t *testing.T) {
	old, config, r := executionFixture(t, 1)
	old.Close()
	p, err := runner.Open("../../adapters")
	if err != nil {
		t.Fatal(err)
	}
	envs, err := p.Environments()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envs["node-sealed-v1"]; !ok {
		t.Skip("Node unavailable")
	}
	config.Runner = p
	s, err := runtime.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := t.TempDir()
	seal := func(contents string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, "sample.test.cjs"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		sealed, err := config.Snapshots.Seal(context.Background(), source, nil)
		if err != nil {
			t.Fatal(err)
		}
		return sealed.SnapshotID
	}
	// Hold the CPU budget with a real process. Both target subscribers attach
	// while queued, so journal/fsync speed cannot turn overlap into a timing bet.
	r.Plan = protocol.Plan{Adapter: "node-v1", TestIDs: []string{"sample.test.cjs"}, Workers: 1}
	r.Environment = envs["node-v1"]
	r.ReusePolicy = "none"
	r.Policy.ExecutionTimeoutMS = 30000
	r.RequestTimeoutMS = 30000
	r.SnapshotID = seal(`const {test}=require('node:test');test('hold',async()=>{await new Promise(r=>setTimeout(r,20000))});`)
	blocker, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	r.SnapshotID = seal(`const {test}=require('node:test');const add=require('./add.cjs');test('real',()=>{require('node:assert/strict').equal(add(2,3),5);console.log('ONE_REAL_EXECUTION');});`)
	if err := os.WriteFile(filepath.Join(source, "add.cjs"), []byte(`module.exports=(a,b)=>a+b;`), 0600); err != nil {
		t.Fatal(err)
	}
	sealed, err := config.Snapshots.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SnapshotID = sealed.SnapshotID
	r.Plan.Adapter = "node-sealed-v1"
	r.Environment = envs[r.Plan.Adapter]
	r.ReusePolicy = "inflight"
	r.IdempotencyKey = "shared-A"
	a, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	r.CallerRef = "B"
	r.IdempotencyKey = "shared-B"
	b, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestID == b.RequestID || a.JobID != b.JobID {
		t.Fatalf("real requests were not coalesced: %+v %+v", a, b)
	}
	// Same paths, different uncommitted bytes: a distinct job before A/B finish.
	if err := os.WriteFile(filepath.Join(source, "add.cjs"), []byte(`module.exports=(a,b)=>a+b+1;`), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := config.Snapshots.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SnapshotID = changed.SnapshotID
	r.CallerRef = "C"
	r.IdempotencyKey = "changed"
	c, err := s.SubmitExecution(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.JobID == a.JobID {
		t.Fatal("different sealed bytes shared a real execution")
	}
	if err := s.Cancel(a.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(blocker.RequestID); err != nil {
		t.Fatal(err)
	}
	passed := waitResult(t, s, b.RequestID)
	failed := waitResult(t, s, c.RequestID)
	cancelled, err := s.ExecutionResult(a.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" || passed.Outcome != "passed" || failed.Outcome != "failed" || passed.CleanupStatus != "confirmed" {
		t.Fatalf("wrong real subscriber results: cancelled=%+v original=%+v changed=%+v", cancelled, passed, failed)
	}
	events, err := s.Events(a.JobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, event := range events.Events {
		if event.Kind == "attempt_started" {
			starts++
		}
	}
	output, err := os.ReadFile(filepath.Join(passed.Workspace, ".focus-test", "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || strings.Count(string(output), "ONE_REAL_EXECUTION") != 1 {
		t.Fatalf("executed repeatedly: starts=%d log=%s", starts, output)
	}
	if cancelled.AttemptID != passed.AttemptID || cancelled.ResultDigest != passed.ResultDigest {
		t.Fatal("subscriber rewrote original attempt or evidence identity")
	}
}
