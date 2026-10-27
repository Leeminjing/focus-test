package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"focus-test-runtime/internal/protocol"
)

type AdapterEvent struct {
	Version  int    `json:"version"`
	Seq      int    `json:"seq"`
	Kind     string `json:"kind"`
	TestID   string `json:"test_id,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Message  string `json:"message,omitempty"`
	Count    int    `json:"count,omitempty"`
	Complete bool   `json:"complete,omitempty"`
}

type Outcome struct {
	ExitCode       int    `json:"exit_code"`
	Cleanup        string `json:"cleanup_status"`
	ReportComplete bool   `json:"report_complete"`
	Passed         int    `json:"passed"`
	Skipped        int    `json:"skipped"`
	Err            error  `json:"-"`
}

type adapter struct {
	executable string
	script     string
}
type Provider struct{ adapters map[string]adapter }

func Open(adapterDirectory string) (*Provider, error) {
	abs, err := filepath.Abs(adapterDirectory)
	if err != nil {
		return nil, err
	}
	p := &Provider{adapters: map[string]adapter{}}
	for id, item := range map[string]struct{ binary, script string }{"node-v1": {"node", "node/reporter.cjs"}, "pytest-v1": {"python", "pytest/worker.py"}} {
		executable, err := exec.LookPath(item.binary)
		if err != nil {
			continue
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return nil, err
		}
		script := filepath.Join(abs, filepath.FromSlash(item.script))
		if _, err := os.Stat(script); err != nil {
			return nil, err
		}
		p.adapters[id] = adapter{executable: executable, script: script}
	}
	return p, nil
}

func (p *Provider) Environments() (map[string]protocol.Environment, error) {
	envs := map[string]protocol.Environment{}
	for id := range p.adapters {
		env, _, err := p.environment(id)
		if err != nil {
			return nil, err
		}
		envs[id] = env
	}
	return envs, nil
}

func (p *Provider) environment(id string) (protocol.Environment, []string, error) {
	a, ok := p.adapters[id]
	if !ok {
		return protocol.Environment{}, nil, errors.New("adapter executable unavailable")
	}
	runner, err := fileDigest(a.executable)
	if err != nil {
		return protocol.Environment{}, nil, err
	}
	script, err := fileDigest(a.script)
	if err != nil {
		return protocol.Environment{}, nil, err
	}
	values := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name != "" {
			values[name] = value
		}
	}
	delete(values, "FOCUS_TEST_EVENTS")
	if id == "pytest-v1" {
		values["PYTEST_DISABLE_PLUGIN_AUTOLOAD"] = "1"
	}
	variables := map[string]string{}
	actual := make([]string, 0, len(values))
	for name, value := range values {
		variables[name] = hash([]byte(value))
		actual = append(actual, name+"="+value)
	}
	sort.Strings(actual)
	return protocol.Environment{OS: runtime.GOOS, Arch: runtime.GOARCH, RunnerDigest: runner,
		Variables: variables, Data: map[string]string{"adapter": script, "runner_location": hash([]byte(a.executable))}}, actual, nil
}

func (p *Provider) Validate(spec protocol.ExecutionRequest) error {
	if spec.Policy.Isolation != "local-process" || spec.Policy.Network != "declared" {
		return errors.New("local runner cannot enforce the requested sandbox policy")
	}
	if spec.ReusePolicy != "none" {
		return errors.New("real adapters require reuse_policy none until external dependency closure is verified")
	}
	if len(spec.Plan.Args) != 0 {
		return errors.New("extra runner arguments are not supported in adapter v1")
	}
	if spec.Plan.Seed != 0 {
		return errors.New("nonzero random seeds are not supported in adapter v1")
	}
	if spec.Plan.Adapter == "pytest-v1" && spec.Plan.Workers != 1 {
		return errors.New("pytest-v1 preserves fixture scope in one process")
	}
	env, _, err := p.environment(spec.Plan.Adapter)
	if err != nil {
		return err
	}
	// Nil and empty maps represent the same empty manifest.
	a, _ := json.Marshal(env)
	b, _ := json.Marshal(spec.Environment)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	if !reflect.DeepEqual(x, y) {
		return errors.New("execution environment changed or is not this provider's environment")
	}
	return nil
}

func (p *Provider) Run(ctx context.Context, spec protocol.ExecutionRequest, workspace string, emit func(AdapterEvent) error) Outcome {
	result := Outcome{ExitCode: -1, Cleanup: "not_started"}
	if err := p.Validate(spec); err != nil {
		result.Err = err
		return result
	}
	_, environment, err := p.environment(spec.Plan.Adapter)
	if err != nil {
		result.Err = err
		return result
	}
	scratch := filepath.Join(workspace, ".focus-test")
	if err := os.Mkdir(scratch, 0700); err != nil {
		result.Err = err
		return result
	}
	eventPath := filepath.Join(scratch, "adapter.jsonl")
	channel, err := os.OpenFile(eventPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		result.Err = err
		return result
	}
	defer channel.Close()
	stdout, err := os.Create(filepath.Join(scratch, "stdout.log"))
	if err != nil {
		result.Err = err
		return result
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(scratch, "stderr.log"))
	if err != nil {
		result.Err = err
		return result
	}
	defer stderr.Close()
	a := p.adapters[spec.Plan.Adapter]
	args := []string{}
	if spec.Plan.Adapter == "node-v1" {
		reporterPath := filepath.ToSlash(a.script)
		if !strings.HasPrefix(reporterPath, "/") {
			reporterPath = "/" + reporterPath
		}
		reporterURL := (&url.URL{Scheme: "file", Path: reporterPath}).String()
		args = []string{"--test", fmt.Sprintf("--test-concurrency=%d", spec.Plan.Workers), "--test-reporter=" + reporterURL}
	} else {
		args = []string{a.script, "-q", "--override-ini=addopts="}
	}
	args = append(args, spec.Plan.TestIDs...)
	environment = append(environment, "FOCUS_TEST_EVENTS="+eventPath)
	controlled, cancel := context.WithCancel(ctx)
	defer cancel()
	proc, err := start(command{Executable: a.executable, Args: args, Directory: workspace, Environment: environment, Stdout: stdout, Stderr: stderr})
	if err != nil {
		result.Err = err
		return result
	}
	result.Cleanup = "unknown"
	done := make(chan processResult, 1)
	go func() { done <- proc.Wait(controlled) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var pending []byte
	seq, finalCount := 0, 0
	final := false
	cases := map[string]bool{}
	covered := map[string]bool{}
	readEvents := func() error {
		buffer := make([]byte, 32<<10)
		for {
			n, err := channel.Read(buffer)
			pending = append(pending, buffer[:n]...)
			if len(pending) > 64<<10 {
				return errors.New("adapter event line too large")
			}
			for {
				at := bytes.IndexByte(pending, '\n')
				if at < 0 {
					break
				}
				line := append([]byte(nil), pending[:at]...)
				pending = pending[at+1:]
				var event AdapterEvent
				if json.Unmarshal(line, &event) != nil || event.Version != 1 || event.Seq != seq+1 || final {
					return errors.New("adapter event sequence or protocol incomplete")
				}
				seq++
				switch event.Kind {
				case "case_failed":
					if event.TestID == "" {
						return errors.New("failure without test identity")
					}
				case "case_finished":
					if event.TestID == "" || cases[event.TestID] {
						return errors.New("duplicate or missing final case identity")
					}
					cases[event.TestID] = true
					for _, scope := range spec.Plan.TestIDs {
						if event.TestID == scope || strings.HasPrefix(event.TestID, scope+"::") || strings.HasPrefix(event.TestID, scope+"[") {
							covered[scope] = true
						}
					}
					finalCount++
					if event.Outcome == "passed" {
						result.Passed++
					} else if event.Outcome == "skipped" {
						result.Skipped++
					} else if event.Outcome != "failed" {
						return errors.New("invalid case outcome")
					}
				case "report_finished":
					final = true
					result.ReportComplete = event.Complete && event.Count == finalCount && finalCount > 0 && len(covered) == len(spec.Plan.TestIDs)
				default:
					return errors.New("unknown adapter event")
				}
				if err := emit(event); err != nil {
					return err
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	var protocolErr error
	for {
		select {
		case processResult := <-done:
			if protocolErr == nil {
				protocolErr = readEvents()
			}
			protocolErr = errors.Join(protocolErr, outputQuota(stdout, stderr, channel))
			if len(pending) != 0 || !final {
				protocolErr = errors.Join(protocolErr, errors.New("adapter report not closed"))
			}
			result.ExitCode, result.Cleanup = processResult.ExitCode, processResult.Cleanup
			result.Err = errors.Join(protocolErr, processResult.Err)
			if result.Err != nil {
				result.ReportComplete = false
			}
			return result
		case <-ticker.C:
			if protocolErr == nil {
				protocolErr = readEvents()
				protocolErr = errors.Join(protocolErr, outputQuota(stdout, stderr, channel))
				if protocolErr != nil {
					cancel()
				}
			}
		}
	}
}

func fileDigest(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func outputQuota(files ...*os.File) error {
	for _, f := range files {
		if info, err := f.Stat(); err != nil || info.Size() > 4<<20 {
			return errors.New("attempt output quota exceeded")
		}
	}
	return nil
}
func hash(value []byte) string {
	d := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(d[:])
}
