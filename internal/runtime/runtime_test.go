package runtime_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
)

func request(key string, ids ...string) protocol.FixtureRequest {
	return protocol.FixtureRequest{ProtocolVersion: protocol.Version, IdempotencyKey: key, TestIDs: ids}
}

func TestConcurrentRetryIsOneSubmission(t *testing.T) {
	s := runtime.New()
	t.Cleanup(s.Close)
	var wg sync.WaitGroup
	ids := make(chan string, 32)
	for range 32 {
		wg.Go(func() {
			accepted, err := s.Submit(request("same-key", "fixture.pass"))
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			ids <- accepted.JobID
		})
	}
	wg.Wait()
	close(ids)
	unique := make(map[string]bool)
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("concurrent retries created %d jobs", len(unique))
	}
	if _, err := s.Submit(request("same-key", "fixture.fail")); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("changed retry must conflict, got %v", err)
	}
	other, err := s.Submit(request("different-key", "fixture.pass"))
	if err != nil || unique[other.JobID] {
		t.Fatalf("different requests must remain independent in bootstrap: %+v, %v", other, err)
	}
}

func TestFailureAvailableBeforeJobCompletes(t *testing.T) {
	s := runtime.New()
	t.Cleanup(s.Close)
	accepted, err := s.Submit(request("early-failure", "fixture.fail", "fixture.slow"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	cursor := 0
	sawEarlyFailure := false
	for time.Now().Before(deadline) {
		page, err := s.Events(accepted.JobID, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			if event.Seq != cursor+1 || event.JobID != accepted.JobID {
				t.Fatalf("broken event order: cursor=%d event=%+v", cursor, event)
			}
			cursor = event.Seq
			if event.Kind == "case_failed" && !page.Terminal {
				sawEarlyFailure = true
			}
		}
		if page.Terminal {
			result, err := s.Result(accepted.JobID)
			if err != nil {
				t.Fatal(err)
			}
			if !sawEarlyFailure || result.Outcome != "failed" || result.Completed != 2 || result.Failed != 1 || result.Mode != protocol.Simulation {
				t.Fatalf("incorrect streamed result: early=%v result=%+v", sawEarlyFailure, result)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fixture job did not finish")
}

func TestCloseSettlesQueuedWorkAndRejectsSubmission(t *testing.T) {
	s := runtime.New()
	var ids []string
	for i := range 3 {
		accepted, err := s.Submit(request(fmt.Sprint(i), "fixture.slow"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, accepted.JobID)
	}
	s.Close()
	s.Close()
	for _, id := range ids {
		result, err := s.Result(id)
		if err != nil || result.Status != "cancelled" || result.Outcome != "inconclusive" {
			t.Fatalf("shutdown left unsettled job: %+v, %v", result, err)
		}
		page, err := s.Events(id, 0)
		if err != nil || !page.Terminal || page.Events[len(page.Events)-1].Kind != "job_finished" {
			t.Fatalf("missing terminal event: %+v, %v", page, err)
		}
	}
	if _, err := s.Submit(request("late", "fixture.pass")); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("closed runtime accepted work: %v", err)
	}
}

func TestResultDoesNotExposeMutableState(t *testing.T) {
	s := runtime.New()
	t.Cleanup(s.Close)
	req := request("copy", "fixture.pass")
	accepted, err := s.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	req.TestIDs[0] = "fixture.fail"
	first, _ := s.Result(accepted.JobID)
	first.TestIDs[0] = "fixture.slow"
	second, _ := s.Result(accepted.JobID)
	if second.TestIDs[0] != "fixture.pass" {
		t.Fatal("caller mutated accepted test plan")
	}
}

func TestRetentionBoundAndIdempotentRetryAtCapacity(t *testing.T) {
	s := runtime.New()
	t.Cleanup(s.Close)
	for i := range runtime.MaxJobs {
		if _, err := s.Submit(request(fmt.Sprint(i), "fixture.slow")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Submit(request("overflow", "fixture.slow")); !errors.Is(err, runtime.ErrCapacity) {
		t.Fatalf("expected bounded retention, got %v", err)
	}
	if _, err := s.Submit(request("0", "fixture.slow")); err != nil {
		t.Fatalf("retry at capacity should recover existing request: %v", err)
	}
}
