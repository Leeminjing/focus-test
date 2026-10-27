package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runner"
	"focus-test-runtime/internal/runtime"
)

func TestRealExecutionBindsSealedInputs(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"pass", `const {test}=require('node:test');test('pass',()=>{});`, "passed"},
		{"mutated-input", `const {test}=require('node:test');const fs=require('node:fs');test('mutation',()=>fs.writeFileSync('sample.test.cjs','changed'));`, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, config, request := executionFixture(t, 1)
			s.Close()
			provider, err := runner.Open("../../adapters")
			if err != nil {
				t.Fatal(err)
			}
			environments, err := provider.Environments()
			if err != nil {
				t.Fatal(err)
			}
			env, ok := environments["node-v1"]
			if !ok {
				t.Skip("Node unavailable")
			}
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "sample.test.cjs"), []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			sealed, err := config.Snapshots.Seal(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.SnapshotID = sealed.SnapshotID
			request.Plan = protocol.Plan{Adapter: "node-v1", TestIDs: []string{"sample.test.cjs"}, Workers: 1}
			request.Environment = env
			request.ReusePolicy = "none"
			config.Runner = provider
			real, err := runtime.Open(config)
			if err != nil {
				t.Fatal(err)
			}
			defer real.Close()
			bound, err := real.SubmitExecution(request)
			if err != nil {
				t.Fatal(err)
			}
			result := waitResult(t, real, bound.RequestID)
			if result.Outcome != tc.want || result.Mode != "local-process" || result.SnapshotID != sealed.SnapshotID || result.ResultDigest == "" || result.CleanupStatus != "confirmed" {
				t.Fatalf("incorrect sealed execution: %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(source, "sample.test.cjs"))
			if err != nil || string(data) != tc.source {
				t.Fatal("execution modified caller's workspace")
			}
			request.ReusePolicy = "inflight"
			request.IdempotencyKey = "unsafe-reuse"
			if _, err := real.SubmitExecution(request); err == nil {
				t.Fatal("real dependency closure was assumed rather than verified")
			}
		})
	}
}
