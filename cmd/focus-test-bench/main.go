// focus-test-bench measures simulated context coordination. It is not a
// benchmark of real Focus model consumption or application test acceleration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	platform "runtime"
	"time"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/snapshot"
)

type sample struct {
	Contexts       int     `json:"contexts"`
	Reuse          string  `json:"reuse"`
	CPUUnits       int     `json:"cpu_units"`
	MemoryMiB      int     `json:"memory_mib"`
	ExecutedJobs   int     `json:"executed_jobs"`
	Milliseconds   float64 `json:"completion_ms"`
	FirstFailureMS float64 `json:"first_observed_failure_ms"`
}

func main() {
	output := flag.String("output", "", "optional JSON output file")
	revision := flag.String("revision", "unrecorded", "source revision used for this measurement")
	flag.Parse()
	if err := run(*output, *revision); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output, revision string) error {
	directory, err := os.MkdirTemp("", "focus-test-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	source := filepath.Join(directory, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "fixture.txt"), []byte("benchmark-input-v1"), 0600); err != nil {
		return err
	}
	store, err := snapshot.Open(filepath.Join(directory, "snapshots"))
	if err != nil {
		return err
	}
	defer store.Close()
	before := time.Now()
	sealed, err := store.Seal(context.Background(), source, nil)
	if err != nil {
		return err
	}
	cold := float64(time.Since(before).Microseconds()) / 1000
	before = time.Now()
	_, err = store.Seal(context.Background(), source, nil)
	if err != nil {
		return err
	}
	warm := float64(time.Since(before).Microseconds()) / 1000
	var samples []sample
	for _, contexts := range []int{1, 2, 4, 8} {
		for _, reuse := range []string{"none", "inflight"} {
			measurement, err := measure(store, sealed.SnapshotID, contexts, reuse)
			if err != nil {
				return err
			}
			samples = append(samples, measurement)
		}
	}
	report := struct {
		Revision   string   `json:"source_revision"`
		OS         string   `json:"os"`
		Arch       string   `json:"arch"`
		GoVersion  string   `json:"go_version"`
		Mode       string   `json:"mode"`
		Input      string   `json:"input"`
		ColdSealMS float64  `json:"cold_seal_ms"`
		WarmSealMS float64  `json:"warm_seal_ms"`
		Samples    []sample `json:"samples"`
		Limits     []string `json:"limits"`
	}{revision, platform.GOOS, platform.GOARCH, platform.Version(), "simulation", "presealed", cold, warm, samples, []string{"single repetition per cell", "in-memory events; durable journal latency excluded", "no real application or Focus model consumption measurement", "no persistent result cache", "no CPU-time or peak-memory measurement"}}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if output != "" {
		if err := os.WriteFile(output, data, 0600); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(data)
	return err
}

func measure(store *snapshot.Store, id string, count int, reuse string) (sample, error) {
	s, err := runtime.Open(runtime.Config{Snapshots: store, CPUUnits: 2, MemoryMiB: 512})
	if err != nil {
		return sample{}, err
	}
	defer s.Close()
	r := protocol.ExecutionRequest{ProtocolVersion: 1, IdempotencyKey: "benchmark", TrustDomain: "benchmark", SnapshotID: id,
		Plan: protocol.Plan{Adapter: "fixture-v1", TestIDs: []string{"fixture.fail", "fixture.slow"}, Workers: 1}, Environment: runtime.FixtureEnvironment(),
		Policy: protocol.Policy{ExecutionTimeoutMS: 10000, Isolation: "local-process", Network: "declared"}, Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 15000, ReusePolicy: reuse}
	started := time.Now()
	deadline := started.Add(15 * time.Second)
	bindings := []protocol.Binding{}
	jobs := map[string]bool{}
	for i := range count {
		r.CallerRef = fmt.Sprintf("context-%d", i)
		b, err := s.SubmitExecution(r)
		if err != nil {
			return sample{}, err
		}
		bindings = append(bindings, b)
		jobs[b.JobID] = true
	}
	firstFailure := float64(-1)
	for time.Now().Before(deadline) {
		all := true
		for _, b := range bindings {
			result, err := s.ExecutionResult(b.RequestID)
			if err != nil {
				return sample{}, err
			}
			if result.Status != "completed" {
				all = false
			}
		}
		if firstFailure < 0 {
			for job := range jobs {
				events, err := s.Events(job, 0)
				if err != nil {
					return sample{}, err
				}
				for _, e := range events.Events {
					if e.Kind == "case_failed" {
						firstFailure = float64(time.Since(started).Microseconds()) / 1000
					}
				}
			}
		}
		if all {
			return sample{count, reuse, 2, 512, len(jobs), float64(time.Since(started).Microseconds()) / 1000, firstFailure}, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return sample{}, errors.New("benchmark requests did not finish")
}
