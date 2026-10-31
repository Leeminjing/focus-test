package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"focus-test-runtime/internal/snapshot"
)

// Opt in with FOCUS_TEST_UPSTREAM: this runs the checked-out Focus graph, not a
// replacement graph implemented in Go. No external model API is invoked.
func TestUpstreamFocusConsumesRealFailureBeforeSlowTestEnds(t *testing.T) {
	upstream := os.Getenv("FOCUS_TEST_UPSTREAM")
	if upstream == "" {
		t.Skip("set FOCUS_TEST_UPSTREAM to a verified Focus checkout for the optional graph probe")
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	dataDir := t.TempDir()
	contents := `const {test}=require('node:test');const assert=require('node:assert/strict');test('parallel',{concurrency:2},async t=>{await Promise.all([t.test('slow declared first',async()=>{await new Promise(r=>setTimeout(r,3000));}),t.test('fast real assertion',()=>assert.equal(2+2,5))]);});`
	if err := os.WriteFile(filepath.Join(source, "sample.test.cjs"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	store, err := snapshot.Open(filepath.Join(dataDir, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := store.Seal(context.Background(), source, nil)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	sealMS := float64(time.Since(before).Microseconds()) / 1000
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		defer writer.Close()
		done <- run(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", dataDir, "--adapter-dir", "../../adapters"}, writer, io.Discard)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		t.Fatal("daemon startup failed")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 4 {
		t.Fatal("invalid daemon startup")
	}
	output := os.Getenv("FOCUS_TEST_FEEDBACK_REPORT")
	if output == "" {
		output = filepath.Join(t.TempDir(), "feedback.json")
	}
	command := exec.CommandContext(ctx, python, "../../integration/focus/verify_feedback.py", "--focus-checkout", upstream,
		"--url", fields[3], "--token-file", filepath.Join(dataDir, "token"), "--snapshot", sealed.SnapshotID, "--workspace", source,
		"--seal-ms", fmt.Sprint(sealMS), "--output", output)
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("actual Focus graph probe: %v\n%s", err, result)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Focus graph feedback report: %s", data)
}
