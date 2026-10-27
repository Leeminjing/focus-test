package protocol_test

import (
	"strings"
	"testing"

	"focus-test-runtime/internal/protocol"
)

func validRequest() protocol.ExecutionRequest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return protocol.ExecutionRequest{
		ProtocolVersion: protocol.Version, IdempotencyKey: "retry", CallerRef: "A", TrustDomain: "project",
		SnapshotID: digest, Plan: protocol.Plan{Adapter: "node-v1", TestIDs: []string{"test/a.test.js"}, Workers: 1},
		Environment: protocol.Environment{OS: "windows", Arch: "amd64", RunnerDigest: digest},
		Policy:      protocol.Policy{ExecutionTimeoutMS: 1000, Isolation: "local-process", Network: "declared"},
		Resources:   protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 2000, ReusePolicy: "inflight",
	}
}

func TestExecutionIdentityAndCallerIndependence(t *testing.T) {
	r := validRequest()
	key, err := r.ExecutionKey()
	if err != nil {
		t.Fatal(err)
	}
	r.CallerRef, r.IdempotencyKey, r.RequestTimeoutMS = "B", "other-retry", 3000
	r.Environment.Variables = map[string]string{}
	r.Plan.Args = []string{}
	other, err := r.ExecutionKey()
	if err != nil || key != other {
		t.Fatalf("caller fields or nil collections changed execution identity: %v", err)
	}
	mutations := map[string]func(*protocol.ExecutionRequest){
		"snapshot":    func(r *protocol.ExecutionRequest) { r.SnapshotID = "sha256:" + strings.Repeat("b", 64) },
		"environment": func(r *protocol.ExecutionRequest) { r.Environment.Arch = "arm64" },
		"policy":      func(r *protocol.ExecutionRequest) { r.Policy.ExecutionTimeoutMS++ },
		"scope":       func(r *protocol.ExecutionRequest) { r.Plan.TestIDs = []string{"test/b.test.js"} },
		"seed":        func(r *protocol.ExecutionRequest) { r.Plan.Seed++ },
		"worker":      func(r *protocol.ExecutionRequest) { r.Plan.Workers = 2; r.Resources.CPUUnits = 2 },
		"domain":      func(r *protocol.ExecutionRequest) { r.TrustDomain = "another-project" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := validRequest()
			mutate(&changed)
			actual, err := changed.ExecutionKey()
			if err != nil || actual == key {
				t.Fatalf("changed execution input reused identity: %v", err)
			}
		})
	}
}

func TestRejectIncompleteOrUnsafeContract(t *testing.T) {
	for name, mutate := range map[string]func(*protocol.ExecutionRequest){
		"version":      func(r *protocol.ExecutionRequest) { r.ProtocolVersion++ },
		"digest":       func(r *protocol.ExecutionRequest) { r.SnapshotID = "HEAD" },
		"zero-scope":   func(r *protocol.ExecutionRequest) { r.Plan.TestIDs = nil },
		"parent-path":  func(r *protocol.ExecutionRequest) { r.Plan.TestIDs = []string{"../elsewhere"} },
		"no-domain":    func(r *protocol.ExecutionRequest) { r.TrustDomain = "" },
		"plain-secret": func(r *protocol.ExecutionRequest) { r.Environment.Variables = map[string]string{"TOKEN": "secret"} },
		"over-budget":  func(r *protocol.ExecutionRequest) { r.Plan.Workers = 2 },
		"zero-timeout": func(r *protocol.ExecutionRequest) { r.Policy.ExecutionTimeoutMS = 0 },
		"persistent":   func(r *protocol.ExecutionRequest) { r.ReusePolicy = "persistent" },
	} {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if _, err := r.ExecutionKey(); err == nil {
				t.Fatal("invalid execution contract accepted")
			}
		})
	}
}
