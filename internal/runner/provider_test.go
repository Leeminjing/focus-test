package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"focus-test-runtime/internal/protocol"
)

func providerFixture(t *testing.T, id, contents string) (*Provider, protocol.ExecutionRequest, string) {
	t.Helper()
	p, err := Open("../../adapters")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.adapters[id]; !ok {
		t.Skip("runtime executable unavailable")
	}
	envs, err := p.Environments()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	name := "sample.test.cjs"
	if id == "pytest-v1" {
		name = "test_sample.py"
	}
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	r := protocol.ExecutionRequest{ProtocolVersion: 1, IdempotencyKey: "test", CallerRef: "A", TrustDomain: "local",
		SnapshotID: hash([]byte("test-input")), Plan: protocol.Plan{Adapter: id, TestIDs: []string{name}, Workers: 1},
		Environment: envs[id], Policy: protocol.Policy{Isolation: "local-process", Network: "declared", ExecutionTimeoutMS: 5000},
		Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 6000, ReusePolicy: "none"}
	return p, r, workspace
}

func TestRealAdapters(t *testing.T) {
	cases := []struct {
		name, id, source string
		wantExit         int
		complete         bool
		failed           int
	}{
		{"node-pass", "node-v1", `const {test}=require('node:test');test('passes',()=>{});`, 0, true, 0},
		{"node-fail", "node-v1", `const {test}=require('node:test');test('fails',()=>{throw Error('assertion failure')});`, 1, true, 1},
		{"node-nested", "node-v1", `const {test,describe}=require('node:test');describe('A',()=>test('same',()=>{}));describe('B',()=>test('same',()=>{}));`, 0, true, 0},
		{"node-zero", "node-v1", `// no named tests`, 0, false, 0},
		{"pytest-pass", "pytest-v1", "def test_pass():\n    assert 1 == 1\n", 0, true, 0},
		{"pytest-teardown", "pytest-v1", "import pytest\n@pytest.fixture\ndef value():\n    yield 1\n    assert False, 'teardown failed'\ndef test_value(value):\n    assert value == 1\n", 1, true, 1},
		{"pytest-setup", "pytest-v1", "import pytest\n@pytest.fixture\ndef value():\n    raise RuntimeError('setup failed')\ndef test_value(value):\n    assert value == 1\n", 1, true, 1},
		{"pytest-collection", "pytest-v1", "def test_broken(\n", 2, false, 0},
		{"pytest-zero", "pytest-v1", "# no tests\n", 5, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, r, workspace := providerFixture(t, tc.id, tc.source)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			failed := 0
			actual := p.Run(ctx, r, workspace, func(e AdapterEvent) error {
				if e.Kind == "case_finished" && e.Outcome == "failed" {
					failed++
				}
				return nil
			})
			if actual.Err != nil || actual.ExitCode != tc.wantExit || actual.ReportComplete != tc.complete || actual.Cleanup != "confirmed" || failed != tc.failed {
				stderr, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "stderr.log"))
				stdout, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "stdout.log"))
				events, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "adapter.jsonl"))
				t.Fatalf("result=%+v failed=%d; stderr=%s stdout=%s events=%s", actual, failed, stderr, stdout, events)
			}
		})
	}
}

func TestMalformedReportAndOutputQuotaCannotPass(t *testing.T) {
	for _, source := range []string{
		`const {test}=require('node:test');test('malformed',()=>{require('node:fs').appendFileSync(process.env.FOCUS_TEST_EVENTS,'not-json\n')});`,
		`const {test}=require('node:test');test('flood',()=>console.log('x'.repeat(6*1024*1024)));`,
	} {
		p, r, workspace := providerFixture(t, "node-v1", source)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		actual := p.Run(ctx, r, workspace, func(e AdapterEvent) error { return nil })
		cancel()
		if actual.Err == nil || actual.ReportComplete || actual.Cleanup != "confirmed" {
			t.Fatalf("invalid evidence was accepted: %+v", actual)
		}
	}
}

func TestAdapterTimeoutAndEarlyFailure(t *testing.T) {
	p, r, workspace := providerFixture(t, "node-v1", `const {test}=require('node:test');test('early',()=>{throw Error('early')});test('slow',async()=>{await new Promise(r=>setTimeout(r,10000))});`)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	early := false
	actual := p.Run(ctx, r, workspace, func(e AdapterEvent) error {
		if e.Kind == "case_failed" && ctx.Err() == nil {
			early = true
		}
		return nil
	})
	if !early || !errors.Is(actual.Err, context.DeadlineExceeded) || actual.ReportComplete || actual.Cleanup != "confirmed" {
		t.Fatalf("timeout/early failure contract violated: early=%v result=%+v", early, actual)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("FOCUS_RUNNER_HELPER") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] == "spawn" {
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "hang")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("FOCUS_CHILD_MARKER"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(3)
		}
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestProcessTreeCancellation(t *testing.T) {
	dir := t.TempDir()
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	marker := filepath.Join(dir, "child.pid")
	proc, err := start(command{Executable: os.Args[0], Args: []string{"-test.run=TestHelperProcess", "--", "spawn"}, Directory: dir,
		Environment: append(os.Environ(), "FOCUS_RUNNER_HELPER=1", "FOCUS_CHILD_MARKER="+marker), Stdout: stdout, Stderr: stderr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan processResult, 1)
	go func() { done <- proc.Wait(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("helper did not create descendant")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	result := <-done
	if !errors.Is(result.Err, context.Canceled) || result.Cleanup != "confirmed" {
		t.Fatalf("descendant cleanup not confirmed: %+v", result)
	}
}
