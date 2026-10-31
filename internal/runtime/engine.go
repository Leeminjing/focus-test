// Package runtime owns requests, shared jobs, admission, and durable transitions.
package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"focus-test-runtime/adapters/fixture"
	"focus-test-runtime/internal/evidence"
	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runner"
	"focus-test-runtime/internal/snapshot"
)

var (
	ErrNotFound    = errors.New("request or job not found")
	ErrConflict    = errors.New("idempotency key already used with a different request")
	ErrCapacity    = errors.New("retained request limit reached")
	ErrClosed      = errors.New("runtime is closed")
	ErrResources   = errors.New("resource_unsatisfiable")
	ErrUnsupported = errors.New("execution provider does not support the requested contract")
	ErrSnapshot    = errors.New("snapshot_unavailable")
	ErrCleanup     = errors.New("cleanup_unconfirmed: admission stopped; inspect retained attempts")
	ErrEventQuery  = errors.New("invalid_event_query")
)

const MaxJobs = 128

type Config struct {
	Snapshots *snapshot.Store
	StateDir  string
	CPUUnits  int
	MemoryMiB int
	Runner    *runner.Provider
}

type job struct {
	ID      string                    `json:"id"`
	Key     string                    `json:"key"`
	Spec    protocol.ExecutionRequest `json:"spec"`
	Legacy  bool                      `json:"legacy"`
	Created time.Time                 `json:"created"`
	Result  protocol.ExecutionResult  `json:"result"`
	Events  []protocol.Event          `json:"events"`
	cancel  context.CancelFunc
}

type requestState struct {
	ID          string    `json:"id"`
	JobID       string    `json:"job_id"`
	Scope       string    `json:"scope"`
	Fingerprint string    `json:"fingerprint"`
	Status      string    `json:"status"`
	Deadline    time.Time `json:"deadline"`
}

// Each journal record is a single atomic state transition, not a copy of the
// entire runtime. Submit publishes job+request together before acknowledging.
type change struct {
	Kind            string               `json:"kind"`
	Job             *job                 `json:"job,omitempty"`
	Request         *requestState        `json:"request,omitempty"`
	JobID           string               `json:"job_id,omitempty"`
	RequestID       string               `json:"request_id,omitempty"`
	AttemptID       string               `json:"attempt_id,omitempty"`
	EventKind       string               `json:"event_kind,omitempty"`
	TestID          string               `json:"test_id,omitempty"`
	Status          string               `json:"status,omitempty"`
	ExecutionStatus string               `json:"execution_status,omitempty"`
	AdapterEvent    *runner.AdapterEvent `json:"adapter_event,omitempty"`
	Summary         *runner.Outcome      `json:"summary,omitempty"`
	Workspace       string               `json:"workspace,omitempty"`
	Diagnostic      string               `json:"diagnostic,omitempty"`
}

type Service struct {
	mu          sync.Mutex
	jobs        map[string]*job
	requests    map[string]*requestState
	keys        map[string]string
	inflight    map[string]string
	queue       []string
	lastCaller  string
	config      Config
	journal     *evidence.Journal
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closed      bool
	fatal       error
	quarantined bool
}

func New() *Service {
	s, err := Open(Config{})
	if err != nil {
		panic(err)
	}
	return s
}

func Open(config Config) (*Service, error) {
	if config.CPUUnits == 0 {
		config.CPUUnits = 1
	}
	if config.MemoryMiB == 0 {
		config.MemoryMiB = 1024
	}
	if config.CPUUnits < 1 || config.MemoryMiB < 1 {
		return nil, ErrResources
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{jobs: map[string]*job{}, requests: map[string]*requestState{}, keys: map[string]string{},
		inflight: map[string]string{}, config: config, ctx: ctx, cancel: cancel}
	if config.StateDir != "" {
		journal, records, err := evidence.Open(config.StateDir)
		if err != nil {
			cancel()
			return nil, err
		}
		s.journal = journal
		for _, record := range records {
			var c change
			if record.Kind != "transition" || json.Unmarshal(record.Data, &c) != nil {
				journal.Close()
				cancel()
				return nil, errors.New("invalid runtime transition")
			}
			if err := s.apply(c); err != nil {
				journal.Close()
				cancel()
				return nil, err
			}
		}
		// Never replay an unclean execution. Simulation has no surviving
		// processes; real jobs retain unknown cleanup and quarantine admission.
		for _, j := range s.jobs {
			if !terminal(j.Result.Status) {
				if err := s.commit(change{Kind: "finished", JobID: j.ID, ExecutionStatus: "interrupted"}); err != nil {
					journal.Close()
					cancel()
					return nil, err
				}
			}
		}
	}
	s.queue = nil
	for _, j := range s.jobs {
		if j.Result.CleanupStatus == "unknown" {
			s.quarantined = true
		}
	}
	s.wg.Go(s.expire)
	return s, nil
}

func FixtureEnvironment() protocol.Environment {
	return protocol.Environment{OS: runtime.GOOS, Arch: runtime.GOARCH, RunnerDigest: digest([]byte("focus-test-runtime/fixture-v1"))}
}

func (s *Service) Submit(r protocol.FixtureRequest) (protocol.Submission, error) {
	if err := r.Validate(); err != nil {
		return protocol.Submission{}, err
	}
	spec := protocol.ExecutionRequest{ProtocolVersion: protocol.Version, IdempotencyKey: r.IdempotencyKey,
		CallerRef: "bootstrap", TrustDomain: "simulation", SnapshotID: digest([]byte("builtin-fixtures")),
		Plan: protocol.Plan{Adapter: "fixture-v1", TestIDs: r.TestIDs, Workers: 1}, Environment: FixtureEnvironment(),
		Policy:    protocol.Policy{ExecutionTimeoutMS: 60000, Isolation: "local-process", Network: "declared"},
		Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 1}, RequestTimeoutMS: 60000, ReusePolicy: "none"}
	bound, err := s.submit(spec, true)
	return protocol.Submission{ProtocolVersion: protocol.Version, Mode: protocol.Simulation, JobID: bound.JobID}, err
}

func (s *Service) SubmitExecution(spec protocol.ExecutionRequest) (protocol.Binding, error) {
	return s.submit(spec, false)
}

func (s *Service) submit(spec protocol.ExecutionRequest, legacy bool) (protocol.Binding, error) {
	key, err := spec.ExecutionKey()
	if err != nil {
		return protocol.Binding{}, err
	}
	if spec.Plan.Adapter == "fixture-v1" {
		if spec.Plan.Workers != 1 || spec.Policy.Isolation != "local-process" || spec.Policy.Network != "declared" || spec.Environment.OS != runtime.GOOS || spec.Environment.Arch != runtime.GOARCH || spec.Environment.RunnerDigest != FixtureEnvironment().RunnerDigest {
			return protocol.Binding{}, ErrUnsupported
		}
		if err := (protocol.FixtureRequest{ProtocolVersion: protocol.Version, IdempotencyKey: "validation", TestIDs: spec.Plan.TestIDs}).Validate(); err != nil {
			return protocol.Binding{}, err
		}
	} else {
		if s.config.Runner == nil {
			return protocol.Binding{}, ErrUnsupported
		}
		if err := s.config.Runner.Validate(spec); err != nil {
			return protocol.Binding{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return protocol.Binding{}, err
	}
	// Store private copies; callers cannot mutate an admitted plan after hashing.
	var owned protocol.ExecutionRequest
	if err := json.Unmarshal(encoded, &owned); err != nil {
		return protocol.Binding{}, err
	}
	spec = owned
	scopeBytes, _ := json.Marshal([]string{spec.TrustDomain, spec.CallerRef, spec.IdempotencyKey})
	scope, fingerprint := string(scopeBytes), digest(encoded)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.Binding{}, ErrClosed
	}
	if s.fatal != nil {
		return protocol.Binding{}, s.fatal
	}
	if priorID := s.keys[scope]; priorID != "" {
		prior := s.requests[priorID]
		if prior.Fingerprint != fingerprint {
			return protocol.Binding{}, ErrConflict
		}
		return protocol.Binding{RequestID: prior.ID, JobID: prior.JobID, AttemptID: s.jobs[prior.JobID].Result.AttemptID}, nil
	}
	if len(s.requests) >= MaxJobs {
		return protocol.Binding{}, ErrCapacity
	}
	if s.quarantined {
		return protocol.Binding{}, ErrCleanup
	}
	if spec.Resources.CPUUnits > s.config.CPUUnits || spec.Resources.MemoryMiB > s.config.MemoryMiB {
		return protocol.Binding{}, ErrResources
	}
	if !legacy {
		if s.config.Snapshots == nil {
			return protocol.Binding{}, errors.New("snapshot store unavailable")
		}
		sealed, err := s.config.Snapshots.Load(spec.SnapshotID)
		if err != nil {
			return protocol.Binding{}, fmt.Errorf("%w: %v", ErrSnapshot, err)
		}
		if spec.Plan.Adapter != "fixture-v1" {
			files := map[string]bool{}
			for _, entry := range sealed.Manifest.Entries {
				if entry.Kind == "file" {
					files[entry.Path] = true
				}
			}
			for _, id := range spec.Plan.TestIDs {
				if !files[strings.Split(id, "::")[0]] {
					return protocol.Binding{}, fmt.Errorf("%w: requested test file is absent from sealed inputs", ErrSnapshot)
				}
			}
		}
	}
	j := s.jobs[s.inflight[key]]
	if spec.ReusePolicy != "inflight" || j == nil || terminal(j.Result.Status) || s.active(j.ID) == 0 {
		j = nil
	}
	var created *job
	if j == nil {
		id := "job-" + rand.Text()
		j = &job{ID: id, Key: key, Spec: spec, Legacy: legacy, Created: time.Now().UTC(),
			Result: protocol.ExecutionResult{ProtocolVersion: protocol.Version, Mode: protocol.Simulation,
				Binding: protocol.Binding{JobID: id}, Status: "queued", Outcome: "inconclusive", ReportStatus: "incomplete", CleanupStatus: "not_started",
				SnapshotID: spec.SnapshotID, ExecutionKey: key, TestIDs: spec.Plan.TestIDs}}
		created = j
		if spec.Plan.Adapter != "fixture-v1" {
			j.Result.Mode = "local-process"
		}
	}
	r := &requestState{ID: "request-" + rand.Text(), JobID: j.ID, Scope: scope, Fingerprint: fingerprint, Status: "attached",
		Deadline: time.Now().Add(time.Duration(spec.RequestTimeoutMS) * time.Millisecond).UTC()}
	if err := s.commit(change{Kind: "submitted", Job: created, Request: r}); err != nil {
		return protocol.Binding{}, err
	}
	s.drain()
	return protocol.Binding{RequestID: r.ID, JobID: j.ID, AttemptID: j.Result.AttemptID}, nil
}

func (s *Service) ExecutionResult(id string) (protocol.ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return protocol.ExecutionResult{}, s.fatal
	}
	r := s.requests[id]
	if r == nil {
		return protocol.ExecutionResult{}, ErrNotFound
	}
	result := s.jobs[r.JobID].Result
	if result.ExitCode != nil {
		value := *result.ExitCode
		result.ExitCode = &value
	}
	result.RequestID = r.ID
	if r.Status == "cancelled" || r.Status == "expired" {
		result.Status = r.Status
		result.Outcome = "inconclusive"
	}
	result.TestIDs = append([]string(nil), result.TestIDs...)
	return result, nil
}

func (s *Service) JobResult(id string) (protocol.ExecutionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return protocol.ExecutionResult{}, s.fatal
	}
	j := s.jobs[id]
	if j == nil {
		return protocol.ExecutionResult{}, ErrNotFound
	}
	result := j.Result
	if result.ExitCode != nil {
		value := *result.ExitCode
		result.ExitCode = &value
	}
	result.TestIDs = append([]string(nil), result.TestIDs...)
	return result, nil
}

func (s *Service) Capabilities() map[string]any {
	environments := map[string]protocol.Environment{"fixture-v1": FixtureEnvironment()}
	if s.config.Runner != nil {
		if actual, err := s.config.Runner.Environments(); err == nil {
			for id, env := range actual {
				environments[id] = env
			}
		}
	}
	return map[string]any{"protocol_version": protocol.Version, "mode": "local-runtime",
		"environments": environments, "environment": FixtureEnvironment(),
		"sealed_inputs": s.config.Snapshots != nil, "durable_events": s.journal != nil,
		"cpu_units": s.config.CPUUnits, "memory_mib": s.config.MemoryMiB}
}

func (s *Service) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return s.fatal
	}
	if s.quarantined {
		return ErrCleanup
	}
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Service) Result(id string) (protocol.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return protocol.Result{}, s.fatal
	}
	j := s.jobs[id]
	if j == nil {
		return protocol.Result{}, ErrNotFound
	}
	r := j.Result
	return protocol.Result{ProtocolVersion: protocol.Version, Mode: r.Mode, JobID: id, Status: r.Status,
		Outcome: r.Outcome, TestIDs: append([]string(nil), r.TestIDs...), Completed: r.Completed, Failed: r.Failed}, nil
}

func (s *Service) Events(id string, after int) (protocol.EventPage, error) {
	return s.ReadEvents(id, protocol.EventQuery{AfterSeq: after, Limit: protocol.DefaultEventLimit})
}

func (s *Service) ReadEvents(id string, query protocol.EventQuery) (protocol.EventPage, error) {
	if err := query.Validate(); err != nil {
		return protocol.EventPage{}, fmt.Errorf("%w: %v", ErrEventQuery, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return protocol.EventPage{}, s.fatal
	}
	j := s.jobs[id]
	if j == nil {
		return protocol.EventPage{}, ErrNotFound
	}
	if query.AfterSeq > len(j.Events) {
		return protocol.EventPage{}, fmt.Errorf("%w: cursor is ahead of this job", ErrEventQuery)
	}
	page := protocol.EventPage{ProtocolVersion: protocol.Version, Mode: j.Result.Mode, Events: []protocol.Event{}, NextSeq: query.AfterSeq}
	// Reserve envelope space (including sequence digit growth and newline).
	bytes := 1024
	for _, e := range j.Events[query.AfterSeq:] {
		encoded, err := json.Marshal(e)
		if err != nil {
			return protocol.EventPage{}, err
		}
		if bytes+len(encoded)+1 > protocol.MaxEventPageBytes || len(page.Events) == query.Limit {
			break
		}
		bytes += len(encoded) + 1
		page.Events = append(page.Events, e)
		page.NextSeq = e.Seq
	}
	if len(page.Events) == 0 && query.AfterSeq < len(j.Events) {
		return protocol.EventPage{}, errors.New("event exceeds page byte limit")
	}
	page.HasMore = page.NextSeq < len(j.Events)
	page.Terminal = terminal(j.Result.Status) && !page.HasMore
	return page, nil
}

func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.requests[id]
	if r == nil {
		return ErrNotFound
	}
	if s.fatal != nil {
		return s.fatal
	}
	if r.Status != "attached" {
		return nil
	}
	if err := s.detach(r, "cancelled"); err != nil {
		return err
	}
	s.drain()
	return nil
}

func (s *Service) detach(r *requestState, status string) error {
	if err := s.commit(change{Kind: "detached", RequestID: r.ID, Status: status}); err != nil {
		return err
	}
	j := s.jobs[r.JobID]
	if s.active(j.ID) == 0 && !terminal(j.Result.Status) {
		s.removeInflight(j)
		if j.cancel != nil {
			j.cancel()
		} else {
			return s.commit(change{Kind: "finished", JobID: j.ID, ExecutionStatus: "cancelled"})
		}
	}
	return nil
}

func (s *Service) active(id string) int {
	count := 0
	for _, r := range s.requests {
		if r.JobID == id && r.Status == "attached" {
			count++
		}
	}
	return count
}

func (s *Service) expire() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			if !s.closed && s.fatal == nil {
				for _, r := range s.requests {
					if r.Status == "attached" && !now.Before(r.Deadline) {
						_ = s.detach(r, "expired")
					}
				}
				s.drain()
			}
			s.mu.Unlock()
		}
	}
}

// drain admits all resources atomically. Caller rotation provides fairness;
// an aged blocked job reserves capacity by stopping younger backfill.
func (s *Service) drain() {
	if s.closed || s.fatal != nil || s.quarantined {
		return
	}
	for {
		cpu, memory := 0, 0
		locks := map[string]bool{}
		for _, j := range s.jobs {
			if j.Result.Status == "running" {
				cpu += j.Spec.Resources.CPUUnits
				memory += j.Spec.Resources.MemoryMiB
				for _, key := range j.Spec.Resources.Exclusive {
					locks[key] = true
				}
			}
		}
		chosen := -1
		seenCallers := map[string]bool{}
		for i, id := range s.queue {
			j := s.jobs[id]
			if j.Result.Status != "queued" {
				continue
			}
			caller := callerScope(j.Spec)
			if seenCallers[caller] {
				continue
			}
			seenCallers[caller] = true
			fits := cpu+j.Spec.Resources.CPUUnits <= s.config.CPUUnits && memory+j.Spec.Resources.MemoryMiB <= s.config.MemoryMiB
			for _, key := range j.Spec.Resources.Exclusive {
				if locks[key] {
					fits = false
				}
			}
			if !fits {
				if time.Since(j.Created) > 200*time.Millisecond {
					break
				}
				continue
			}
			if chosen < 0 {
				chosen = i
			}
			if caller != s.lastCaller {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			return
		}
		id := s.queue[chosen]
		s.queue = append(s.queue[:chosen], s.queue[chosen+1:]...)
		j := s.jobs[id]
		if err := s.commit(change{Kind: "started", JobID: id, AttemptID: "attempt-" + rand.Text()}); err != nil {
			return
		}
		s.lastCaller = callerScope(j.Spec)
		ctx, cancel := context.WithTimeout(s.ctx, time.Duration(j.Spec.Policy.ExecutionTimeoutMS)*time.Millisecond)
		j.cancel = cancel
		s.wg.Go(func() { defer cancel(); s.run(ctx, j) })
	}
}

func (s *Service) run(ctx context.Context, j *job) {
	var err error
	workspace := ""
	var summary *runner.Outcome
	if !j.Legacy {
		workspace, err = s.config.Snapshots.Materialize(ctx, j.Spec.SnapshotID)
	}
	if err == nil && workspace != "" {
		s.mu.Lock()
		err = s.commit(change{Kind: "workspace", JobID: j.ID, Workspace: workspace})
		s.mu.Unlock()
	}
	if err == nil && j.Spec.Plan.Adapter == "fixture-v1" {
		err = fixture.Run(ctx, j.Spec.Plan.TestIDs, func(kind, id string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			_ = s.commit(change{Kind: "case", JobID: j.ID, EventKind: kind, TestID: id})
		})
	} else if err == nil {
		actual := s.config.Runner.Run(ctx, j.Spec, workspace, func(event runner.AdapterEvent) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.commit(change{Kind: "adapter", JobID: j.ID, AdapterEvent: &event})
		})
		summary = &actual
		err = actual.Err
		if actual.Cleanup == "confirmed" {
			if verifyErr := s.config.Snapshots.VerifyWorkspace(context.Background(), j.Spec.SnapshotID, workspace); verifyErr != nil {
				err = errors.Join(err, verifyErr)
				actual.ReportComplete = false
			}
		}
	}
	status := "exited"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "timed_out"
	} else if errors.Is(err, context.Canceled) {
		status = "cancelled"
	} else if err != nil {
		status = "infrastructure_error"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	diagnostic := ""
	if err != nil {
		diagnostic = err.Error()
	}
	_ = s.commit(change{Kind: "finished", JobID: j.ID, ExecutionStatus: status, Summary: summary, Workspace: workspace, Diagnostic: diagnostic})
	if summary != nil && summary.Cleanup == "unknown" {
		s.quarantined = true
		s.cancel()
	}
	s.drain()
}

func (s *Service) commit(c change) error {
	if s.fatal != nil {
		return s.fatal
	}
	if s.journal != nil {
		if err := s.journal.Append("transition", c); err != nil {
			s.fatal = fmt.Errorf("evidence_unavailable: %w", err)
			s.cancel()
			return s.fatal
		}
	}
	if err := s.apply(c); err != nil {
		s.fatal = err
		s.cancel()
		return err
	}
	return nil
}

func (s *Service) apply(c change) error {
	switch c.Kind {
	case "submitted":
		if c.Request == nil || c.Request.ID == "" || s.requests[c.Request.ID] != nil {
			return errors.New("invalid request transition")
		}
		if c.Job != nil {
			j := c.Job
			if j.ID == "" || s.jobs[j.ID] != nil {
				return errors.New("invalid job transition")
			}
			s.jobs[j.ID] = j
			s.queue = append(s.queue, j.ID)
			if j.Spec.ReusePolicy == "inflight" {
				s.inflight[j.Key] = j.ID
			}
			s.addEvent(j, "job_queued", "")
		}
		if s.jobs[c.Request.JobID] == nil {
			return errors.New("request references missing job")
		}
		s.requests[c.Request.ID] = c.Request
		s.keys[c.Request.Scope] = c.Request.ID
	case "detached":
		r := s.requests[c.RequestID]
		if r == nil || (c.Status != "cancelled" && c.Status != "expired") {
			return errors.New("invalid detach transition")
		}
		r.Status = c.Status
	case "started", "workspace", "case", "adapter", "finished":
		j := s.jobs[c.JobID]
		if j == nil {
			return errors.New("transition references missing job")
		}
		switch c.Kind {
		case "workspace":
			if j.Result.Status != "running" || c.Workspace == "" {
				return errors.New("invalid workspace transition")
			}
			j.Result.Workspace = c.Workspace
		case "started":
			if j.Result.Status != "queued" || c.AttemptID == "" {
				return errors.New("invalid start transition")
			}
			j.Result.Status, j.Result.AttemptID = "running", c.AttemptID
			s.addEvent(j, "attempt_started", "")
		case "case":
			if j.Result.Status != "running" || (c.EventKind != "case_failed" && c.EventKind != "case_finished") {
				return errors.New("invalid case transition")
			}
			j.Result.Completed++
			if c.EventKind == "case_failed" {
				j.Result.Failed++
			}
			s.addEvent(j, c.EventKind, c.TestID)
		case "adapter":
			if j.Result.Status != "running" || c.AdapterEvent == nil {
				return errors.New("invalid adapter transition")
			}
			e := c.AdapterEvent
			if e.Kind == "case_finished" {
				j.Result.Completed++
				if e.Outcome == "failed" {
					j.Result.Failed++
				}
				if e.Outcome == "skipped" {
					j.Result.Skipped++
				}
			}
			s.addEvent(j, e.Kind, e.TestID)
			j.Events[len(j.Events)-1].Outcome = e.Outcome
			j.Events[len(j.Events)-1].Message = e.Message
		case "finished":
			if terminal(j.Result.Status) {
				return errors.New("duplicate terminal transition")
			}
			j.Result.Status, j.Result.ExecutionStatus = "completed", c.ExecutionStatus
			j.Result.Outcome, j.Result.CleanupStatus = "inconclusive", "confirmed"
			if c.Workspace != "" {
				j.Result.Workspace = c.Workspace
			}
			j.Result.Diagnostic = c.Diagnostic
			if c.ExecutionStatus == "cancelled" {
				j.Result.Status = "cancelled"
			}
			if j.Spec.Plan.Adapter == "fixture-v1" && c.ExecutionStatus == "exited" && j.Result.Completed == len(j.Spec.Plan.TestIDs) {
				j.Result.ReportStatus = "complete"
				j.Result.Outcome = "passed"
				if j.Result.Failed > 0 {
					j.Result.Outcome = "failed"
				}
			}
			if j.Spec.Plan.Adapter != "fixture-v1" {
				j.Result.CleanupStatus = "not_started"
				if c.ExecutionStatus == "interrupted" {
					j.Result.CleanupStatus = "unknown"
				}
				if c.Summary != nil {
					j.Result.CleanupStatus = c.Summary.Cleanup
					exit := c.Summary.ExitCode
					j.Result.ExitCode = &exit
					if c.Summary.ReportComplete {
						j.Result.ReportStatus = "complete"
					}
					if c.ExecutionStatus == "exited" && c.Summary.Cleanup == "confirmed" && c.Summary.ReportComplete {
						if c.Summary.ExitCode != 0 || j.Result.Failed > 0 {
							j.Result.Outcome = "failed"
						} else if c.Summary.Passed > 0 {
							j.Result.Outcome = "passed"
						}
					}
				}
			}
			s.removeInflight(j)
			for _, r := range s.requests {
				if r.JobID == j.ID && r.Status == "attached" {
					r.Status = "completed"
				}
			}
			s.addEvent(j, "job_finished", "")
			encoded, err := json.Marshal(j.Result)
			if err != nil {
				return err
			}
			j.Result.ResultDigest = digest(encoded)
		}
	default:
		return errors.New("unknown runtime transition")
	}
	return nil
}

func (s *Service) addEvent(j *job, kind, id string) {
	j.Events = append(j.Events, protocol.Event{JobID: j.ID, Seq: len(j.Events) + 1, Kind: kind, TestID: id})
}

func (s *Service) removeInflight(j *job) {
	if s.inflight[j.Key] == j.ID {
		delete(s.inflight, j.Key)
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.closed = true
	s.cancel()
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel()
		}
		if j.Result.Status == "queued" {
			_ = s.commit(change{Kind: "finished", JobID: j.ID, ExecutionStatus: "cancelled"})
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
	if s.journal != nil {
		_ = s.journal.Close()
	}
}

func terminal(status string) bool { return status == "completed" || status == "cancelled" }
func digest(data []byte) string {
	d := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(d[:])
}

func callerScope(spec protocol.ExecutionRequest) string {
	encoded, _ := json.Marshal([]string{spec.TrustDomain, spec.CallerRef})
	return string(encoded)
}
