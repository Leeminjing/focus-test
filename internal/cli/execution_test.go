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
	"focus-test-runtime/internal/localnet"
	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/server"
	"focus-test-runtime/internal/snapshot"
)

func TestSnapshotToAuthenticatedRequest(t *testing.T) {
	directory := t.TempDir()
	storePath := filepath.Join(directory, "snapshots")
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input.txt"), []byte("sealed"), 0600); err != nil {
		t.Fatal(err)
	}
	var sealed snapshot.Sealed
	var out, stderr bytes.Buffer
	if err := cli.Run(context.Background(), []string{"snapshot", "--source", source, "--store", storePath}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &sealed); err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := runtime.Open(runtime.Config{Snapshots: store, StateDir: filepath.Join(directory, "state"), CPUUnits: 1, MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tokenFile := filepath.Join(directory, "token")
	token, err := localnet.EnsureToken(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(server.Authorized(s, token))
	defer api.Close()
	run := func(args ...string) []byte {
		t.Helper()
		out.Reset()
		stderr.Reset()
		base := []string{"--addr", api.URL, "--token-file", tokenFile}
		if err := cli.Run(context.Background(), append(base, args...), &out, &stderr); err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), out.Bytes()...)
	}
	plan := run("plan", "--snapshot", sealed.SnapshotID, "--tests", "fixture.pass", "--caller", "test-context")
	requestFile := filepath.Join(directory, "request.json")
	if err := os.WriteFile(requestFile, plan, 0600); err != nil {
		t.Fatal(err)
	}
	var bound protocol.Binding
	if err := json.Unmarshal(run("submit-execution", "--file", requestFile), &bound); err != nil {
		t.Fatal(err)
	}
	if bound.RequestID == "" || bound.JobID == "" {
		t.Fatal("request and job identity not separated")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var result protocol.ExecutionResult
		if err := json.Unmarshal(run("request", "--request", bound.RequestID), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status == "completed" {
			if result.Outcome != "passed" || result.SnapshotID != sealed.SnapshotID || result.ResultDigest == "" {
				t.Fatalf("wrong result: %+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not settle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
