package runtime_test

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"focus-test-runtime/internal/runtime"
)

func TestRetentionPinsSurviveCompactionAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old, config, r := executionFixture(t, 1)
		old.Close()
		config.Retention = runtime.RetentionPolicy{MaxAge: time.Nanosecond, MinimumAge: time.Nanosecond, Interval: time.Hour, InputGrace: time.Nanosecond}
		s, err := runtime.Open(config)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		r.Plan.TestIDs = []string{"fixture.pass"}
		a, err := s.SubmitExecution(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Reference(a.JobID, "adoption:A", true); err != nil {
			t.Fatal(err)
		}
		before := waitResult(t, s, a.RequestID)
		r.CallerRef = "B"
		r.IdempotencyKey = "B"
		b, err := s.SubmitExecution(r)
		if err != nil {
			t.Fatal(err)
		}
		waitResult(t, s, b.RequestID)
		time.Sleep(time.Millisecond)
		collection, err := s.Collect()
		if err != nil || collection.Jobs != 1 {
			t.Fatalf("wrong collection: %+v %v", collection, err)
		}
		if _, err := s.ExecutionResult(b.RequestID); !errors.Is(err, runtime.ErrNotFound) {
			t.Fatal("unreferenced result remained")
		}
		s.Close()
		restored, err := runtime.Open(config)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		after, err := restored.ExecutionResult(a.RequestID)
		if err != nil || after.ResultDigest != before.ResultDigest || after.AttemptID != before.AttemptID {
			t.Fatalf("retained execution changed: %+v %v", after, err)
		}
		if collection, err := restored.Collect(); err != nil || collection.Jobs != 0 {
			t.Fatalf("lost durable reference: %+v %v", collection, err)
		}
		if err := restored.Reference(a.JobID, "adoption:A", false); err != nil {
			t.Fatal(err)
		}
		if collection, err := restored.Collect(); err != nil || collection.Jobs != 1 {
			t.Fatalf("release did not reclaim: %+v %v", collection, err)
		}
	})
}

func TestRequestCapacityRecoversUnderPressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := runtime.Open(runtime.Config{Retention: runtime.RetentionPolicy{MinimumAge: time.Nanosecond, MaxAge: time.Hour}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for i := 0; i < runtime.MaxJobs+16; i++ {
			b, err := s.Submit(request(fmt.Sprint(i), "fixture.pass"))
			if err != nil {
				t.Fatalf("capacity not recovered at %d: %v", i, err)
			}
			for {
				result, err := s.Result(b.JobID)
				if err != nil {
					t.Fatal(err)
				}
				if result.Status == "completed" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(time.Nanosecond)
		}
	})
}
