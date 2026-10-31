// Real, durable, end-to-end coordination samples. No Focus/model claim.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	platform "runtime"
	"strings"
	"time"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runner"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/snapshot"
)

type sample struct {
	Contexts       int     `json:"contexts"`
	Repetition     int     `json:"repetition"`
	Reuse          string  `json:"reuse"`
	Jobs           int     `json:"executed_jobs"`
	Attempts       int     `json:"executed_attempts"`
	SealMS         float64 `json:"total_seal_ms"`
	FirstFailureMS float64 `json:"first_durable_failure_read_ms"`
	CompleteMS     float64 `json:"completion_including_seals_and_journal_ms"`
}

func main() {
	output := flag.String("output", "", "JSON output file")
	revision := flag.String("revision", "unrecorded", "source revision including dirty suffix if appropriate")
	repeats := flag.Int("repetitions", 3, "repetitions per cell")
	flag.Parse()
	if err := run(*output, *revision, *repeats); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output, revision string, repeats int) error {
	if repeats < 1 || repeats > 100 {
		return errors.New("repetitions must be 1..100")
	}
	p, err := runner.Open("adapters")
	if err != nil {
		return err
	}
	envs, err := p.Environments()
	if err != nil {
		return err
	}
	env, ok := envs["node-sealed-v1"]
	if !ok {
		return errors.New("Node 24+ required")
	}
	samples := []sample{}
	// Alternate order to reduce an always-cold control versus always-warm reuse
	// bias. Each cell has a new store and journal; OS caches are not flushed.
	for repetition := 1; repetition <= repeats; repetition++ {
		for _, count := range []int{1, 2, 4, 8} {
			modes := []string{"none", "inflight"}
			if repetition%2 == 0 {
				modes = []string{"inflight", "none"}
			}
			for _, reuse := range modes {
				s, err := measure(p, env, count, repetition, reuse)
				if err != nil {
					return err
				}
				samples = append(samples, s)
			}
		}
	}
	nodeVersion, _ := exec.Command("node", "--version").Output()
	report := struct {
		Revision string   `json:"source_revision"`
		OS       string   `json:"os"`
		Arch     string   `json:"arch"`
		Go       string   `json:"go_version"`
		Node     string   `json:"node_version"`
		Adapter  string   `json:"adapter"`
		CPU      int      `json:"cpu_units"`
		Memory   int      `json:"memory_mib"`
		Samples  []sample `json:"samples"`
		Limits   []string `json:"limits"`
	}{revision, platform.GOOS, platform.GOARCH, platform.Version(), strings.TrimSpace(string(nodeVersion)), "node-sealed-v1", 2, 512, samples,
		[]string{"self-contained real Node assertion plus 1-second timer, not a production application workload", "each caller seals inputs; synchronous journal commits included", "polling failure delivery to benchmark client; Focus model consumption measured separately", "new content store per cell, OS disk cache not flushed", "no CPU-time, RSS, persistent cache or statistical tail claim"}}
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

func measure(p *runner.Provider, env protocol.Environment, count, repetition int, reuse string) (sample, error) {
	m := sample{Contexts: count, Repetition: repetition, Reuse: reuse, FirstFailureMS: -1}
	dir, err := os.MkdirTemp("", "focus-test-real-bench-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		return m, err
	}
	code := `const {test}=require('node:test');const assert=require('node:assert/strict');test('parallel',{concurrency:2},async t=>{await Promise.all([t.test('slow first',async()=>{await new Promise(r=>setTimeout(r,1000));}),t.test('fast assertion',()=>assert.equal(2+2,5))]);});`
	if err := os.WriteFile(filepath.Join(source, "sample.test.cjs"), []byte(code), 0600); err != nil {
		return m, err
	}
	store, err := snapshot.Open(filepath.Join(dir, "snapshots"))
	if err != nil {
		return m, err
	}
	defer store.Close()
	s, err := runtime.Open(runtime.Config{Snapshots: store, StateDir: filepath.Join(dir, "state"), CPUUnits: 2, MemoryMiB: 512, Runner: p})
	if err != nil {
		return m, err
	}
	defer s.Close()
	r := protocol.ExecutionRequest{ProtocolVersion: 1, IdempotencyKey: "real-benchmark", TrustDomain: "real-benchmark",
		Plan: protocol.Plan{Adapter: "node-sealed-v1", TestIDs: []string{"sample.test.cjs"}, Workers: 1}, Environment: env,
		Policy: protocol.Policy{ExecutionTimeoutMS: 20000, Isolation: "local-process", Network: "declared"}, Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 60000, ReusePolicy: reuse}
	started := time.Now()
	deadline := started.Add(60 * time.Second)
	requests := []protocol.Binding{}
	jobs := map[string]bool{}
	for index := range count {
		before := time.Now()
		sealed, err := store.Seal(context.Background(), source, nil)
		if err != nil {
			return m, err
		}
		m.SealMS += milliseconds(time.Since(before))
		r.SnapshotID = sealed.SnapshotID
		r.CallerRef = fmt.Sprintf("context-%d", index)
		b, err := s.SubmitExecution(r)
		if err != nil {
			return m, err
		}
		requests = append(requests, b)
		jobs[b.JobID] = true
	}
	for time.Now().Before(deadline) {
		all := true
		for _, b := range requests {
			result, err := s.ExecutionResult(b.RequestID)
			if err != nil {
				return m, err
			}
			if result.Status != "completed" {
				all = false
			} else if result.Outcome != "failed" || result.ReportStatus != "complete" || result.CleanupStatus != "confirmed" {
				return m, fmt.Errorf("invalid benchmark evidence: %+v", result)
			}
		}
		if m.FirstFailureMS < 0 {
			for id := range jobs {
				events, err := s.Events(id, 0)
				if err != nil {
					return m, err
				}
				for _, event := range events.Events {
					if event.Kind == "case_failed" || event.Kind == "group_failed" {
						m.FirstFailureMS = milliseconds(time.Since(started))
						break
					}
				}
				if m.FirstFailureMS >= 0 {
					break
				}
			}
		}
		if all {
			m.Jobs = len(jobs)
			m.CompleteMS = milliseconds(time.Since(started))
			for id := range jobs {
				page, err := s.Events(id, 0)
				if err != nil {
					return m, err
				}
				for _, event := range page.Events {
					if event.Kind == "attempt_started" {
						m.Attempts++
					}
				}
			}
			return m, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return m, errors.New("real benchmark timed out")
}

func milliseconds(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }
