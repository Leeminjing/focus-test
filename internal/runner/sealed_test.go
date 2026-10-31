package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSealedNodeDependencyBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		pass         bool
	}{
		{"relative", `const {test}=require('node:test');const assert=require('node:assert/strict');const add=require('./add.cjs');test('real addition',()=>assert.equal(add(2,3),5));`, true},
		{"package", `require('unsealed-package');`, false},
		{"filesystem", `require('node:fs');`, false},
		{"builtin-bypass", `process.getBuiltinModule('fs');`, false},
		{"native-bypass", `process.binding('fs');`, false},
		{"network", `fetch('https://example.invalid');`, false},
		{"subprocess", `require('node:child_process');`, false},
		{"dynamic-esm", `const {test}=require('node:test');test('dynamic',async()=>{await import('node:fs')});`, false},
		{"esm", `const {test}=require('node:test');test('ESM',async()=>{const m=await import('./add.mjs');require('node:assert/strict').equal(m.add(2,3),5)});`, true},
		{"ambiguous-js", `require('./ambiguous.js');`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, r, workspace := providerFixture(t, "node-sealed-v1", tc.source)
			r.ReusePolicy = "inflight"
			if err := os.WriteFile(filepath.Join(workspace, "add.cjs"), []byte(`module.exports=(a,b)=>a+b;`), 0600); err != nil {
				t.Fatal(err)
			}
			for name, code := range map[string]string{"add.mjs": `export const add=(a,b)=>a+b;`, "ambiguous.js": `module.exports=1;`} {
				if err := os.WriteFile(filepath.Join(workspace, name), []byte(code), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			actual := p.Run(ctx, r, workspace, func(AdapterEvent) error { return nil })
			if actual.Err != nil || actual.Cleanup != "confirmed" || (actual.ExitCode == 0) != tc.pass || (tc.pass && !actual.ReportComplete) {
				stderr, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "stderr.log"))
				stdout, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "stdout.log"))
				events, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "adapter.jsonl"))
				t.Fatalf("result=%+v stderr=%s stdout=%s events=%s", actual, stderr, stdout, events)
			}
		})
	}
}

func TestSealedNodeCanonicalWorkspace(t *testing.T) {
	p, r, workspace := providerFixture(t, "node-sealed-v1", `const {test}=require('node:test');const assert=require('node:assert/strict');test('linked workspace',()=>assert.equal(require('./value.cjs'),42));`)
	if err := os.WriteFile(filepath.Join(workspace, "value.cjs"), []byte(`module.exports=42;`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("host cannot create directory links: %v", err)
		}
		// Windows junction creation needs no symlink privilege. Both targets
		// are owned temporary directories; pass paths as environment data.
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:FOCUS_TEST_ALIAS -Target $env:FOCUS_TEST_TARGET | Out-Null`)
		cmd.Env = append(os.Environ(), "FOCUS_TEST_ALIAS="+alias, "FOCUS_TEST_TARGET="+workspace)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create test path alias: %v %s", err, output)
		}
	}
	r.ReusePolicy = "inflight"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := p.Run(ctx, r, alias, func(AdapterEvent) error { return nil })
	if result.Err != nil || result.ExitCode != 0 || !result.ReportComplete {
		events, _ := os.ReadFile(filepath.Join(workspace, ".focus-test", "adapter.jsonl"))
		t.Fatalf("path alias changed sealed dependency permissions: %+v events=%s", result, events)
	}
}

func TestSealedNodeDoesNotInheritPreloadOrHostSecrets(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--require=unsealed-preload.cjs")
	t.Setenv("FOCUS_TEST_HOST_SECRET", "must not reach sealed tests")
	p, r, workspace := providerFixture(t, "node-sealed-v1", `const {test}=require('node:test');const assert=require('node:assert/strict');test('clean environment',()=>{assert.equal(process.env.NODE_OPTIONS,undefined);assert.equal(process.env.FOCUS_TEST_HOST_SECRET,undefined);});`)
	r.ReusePolicy = "inflight"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := p.Run(ctx, r, workspace, func(AdapterEvent) error { return nil })
	if result.Err != nil || result.ExitCode != 0 || !result.ReportComplete {
		t.Fatalf("unsealed host environment influenced execution: %+v", result)
	}
}
