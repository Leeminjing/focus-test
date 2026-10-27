package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"focus-test-runtime/internal/cli"
	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/server"
)

func TestCLIToDaemon(t *testing.T) {
	service := runtime.New()
	t.Cleanup(service.Close)
	daemon := httptest.NewServer(server.New(service))
	t.Cleanup(daemon.Close)
	file := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(file, []byte(`{"protocol_version":1,"idempotency_key":"cli","test_ids":["fixture.pass"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		if err := cli.Run(context.Background(), append([]string{"--addr", daemon.URL}, args...), &out, &stderr); err != nil {
			t.Fatalf("CLI failed: %v; stderr=%s", err, stderr.String())
		}
		return out.Bytes()
	}
	var accepted protocol.Submission
	if err := json.Unmarshal(run("submit", "--file", file), &accepted); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var result protocol.Result
		if err := json.Unmarshal(run("result", "--job", accepted.JobID), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status == "completed" {
			if result.Outcome != "passed" || result.Mode != protocol.Simulation || result.Completed != 1 {
				t.Fatalf("incorrect result: %+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI result never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var events protocol.EventPage
	if err := json.Unmarshal(run("events", "--job", accepted.JobID, "--after", "0"), &events); err != nil {
		t.Fatal(err)
	}
	if !events.Terminal || len(events.Events) != 4 {
		t.Fatalf("incomplete event page: %+v", events)
	}
	var resumed protocol.EventPage
	if err := json.Unmarshal(run("events", "--job", accepted.JobID, "--after", "2"), &resumed); err != nil {
		t.Fatal(err)
	}
	if len(resumed.Events) != 2 || resumed.Events[0].Seq != 3 {
		t.Fatalf("incorrect cursor replay: %+v", resumed)
	}
}

func TestBadCommandReturnsError(t *testing.T) {
	for _, args := range [][]string{
		nil, {"unknown"}, {"submit"}, {"result"}, {"version", "extra"},
		{"--addr", "http://192.0.2.1:7337", "health"},
		{"--addr", "https://127.0.0.1:7337", "health"},
		{"events", "--job", "fixture-x", "--after", "-1"},
	} {
		var out, stderr bytes.Buffer
		if err := cli.Run(context.Background(), args, &out, &stderr); err == nil {
			t.Errorf("expected command error for %v", args)
		}
	}
}
