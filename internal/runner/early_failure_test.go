package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParallelFailureDoesNotWaitForEarlierSlowFile(t *testing.T) {
	p, request, workspace := providerFixture(t, "node-v1", "// unused fixture")
	slow := `const {test}=require('node:test');const fs=require('node:fs');test('slow first',async()=>{while(!fs.existsSync('.focus-test/release'))await new Promise(r=>setTimeout(r,10));});`
	fast := `const {test}=require('node:test');const fs=require('node:fs');test('fast failure',()=>{fs.writeFileSync('.focus-test/failed','');throw Error('fast failure');});`
	for name, source := range map[string]string{"a-slow.test.cjs": slow, "z-fast.test.cjs": fast} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request.Plan.TestIDs = []string{"a-slow.test.cjs", "z-fast.test.cjs"}
	request.Plan.Workers = 2
	request.Resources.CPUUnits = 2
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failures := make(chan AdapterEvent, 8)
	done := make(chan Outcome, 1)
	go func() {
		done <- p.Run(ctx, request, workspace, func(e AdapterEvent) error {
			if e.Kind == "case_failed" || e.Kind == "group_failed" {
				failures <- e
			}
			return nil
		})
	}()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	early := false
	select {
	case event := <-failures:
		early = strings.Contains(event.TestID, "z-fast.test.cjs")
	case <-deadline.C:
	}
	// Release only after checking feedback. This catches ordering, not a chosen
	// millisecond speed threshold, and always lets the child processes finish.
	if err := os.WriteFile(filepath.Join(workspace, ".focus-test", "release"), nil, 0600); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	result := <-done
	if !early {
		t.Fatalf("failure was withheld until the earlier slow file was released; result=%+v", result)
	}
	if result.Err != nil || !result.ReportComplete || result.ExitCode != 1 {
		t.Fatalf("early feedback damaged final settlement: %+v", result)
	}
	caseFailures, groupFailures := 0, 0
	// The first diagnostic may describe a file (older Node isolation), followed
	// by its named case. Neither may be announced twice or counted twice.
	data, err := os.ReadFile(filepath.Join(workspace, ".focus-test", "adapter.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event AdapterEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "case_failed" {
			caseFailures++
		}
		if event.Kind == "group_failed" {
			groupFailures++
		}
	}
	if caseFailures != 1 || groupFailures > 1 {
		t.Fatalf("duplicate failure diagnostics: %s", data)
	}
}

func TestRepeatedNodeDeclarationsKeepCompletionIdentity(t *testing.T) {
	p, r, workspace := providerFixture(t, "node-v1", `const {test}=require('node:test');test('parent',{concurrency:2},async t=>{await Promise.all([0,1].map(i=>t.test('same',async()=>{if(i===0)await new Promise(r=>setTimeout(r,20));else throw Error('one failure');})));});`)
	announced, settled := map[string]bool{}, map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := p.Run(ctx, r, workspace, func(e AdapterEvent) error {
		if e.Kind == "case_failed" {
			if announced[e.TestID] {
				t.Errorf("duplicate failure: %s", e.TestID)
			}
			announced[e.TestID] = true
		}
		if e.Kind == "case_finished" && e.Outcome == "failed" {
			settled[e.TestID] = true
		}
		return nil
	})
	if result.Err != nil || !result.ReportComplete || result.ExitCode != 1 || len(settled) != 2 || len(announced) != len(settled) {
		t.Fatalf("incorrect nested settlement: %+v announcements=%v settled=%v", result, announced, settled)
	}
	for id := range announced {
		if !settled[id] {
			t.Fatalf("early identity was lost at settlement: %s", id)
		}
	}
}
