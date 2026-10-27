// Package runtime owns bootstrap fixture jobs. This bounded, in-memory service
// intentionally provides neither persistent evidence nor cross-request reuse.
package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"focus-test-runtime/adapters/fixture"
	"focus-test-runtime/internal/protocol"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrConflict = errors.New("idempotency key already used with a different request")
	ErrCapacity = errors.New("simulation job limit reached; restart the daemon to reset")
	ErrClosed   = errors.New("runtime is closed")
)

// MaxJobs bounds both retained results and queued work in this bootstrap.
const MaxJobs = 128

type job struct {
	result protocol.Result
	events []protocol.Event
}

type submission struct {
	digest [32]byte
	jobID  string
}

type Service struct {
	mu     sync.Mutex
	jobs   map[string]*job
	keys   map[string]submission
	queue  chan string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

func New() *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		jobs: make(map[string]*job), keys: make(map[string]submission),
		queue: make(chan string, MaxJobs), ctx: ctx, cancel: cancel,
	}
	s.wg.Add(1)
	go s.work()
	return s
}

func (s *Service) Submit(request protocol.FixtureRequest) (protocol.Submission, error) {
	if err := request.Validate(); err != nil {
		return protocol.Submission{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return protocol.Submission{}, err
	}
	digest := sha256.Sum256(encoded)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.Submission{}, ErrClosed
	}
	if prior, ok := s.keys[request.IdempotencyKey]; ok {
		if prior.digest != digest {
			return protocol.Submission{}, ErrConflict
		}
		return protocol.Submission{ProtocolVersion: protocol.Version, Mode: protocol.Simulation, JobID: prior.jobID}, nil
	}
	if len(s.jobs) >= MaxJobs {
		return protocol.Submission{}, ErrCapacity
	}
	id := "fixture-" + rand.Text()
	j := &job{result: protocol.Result{
		ProtocolVersion: protocol.Version, Mode: protocol.Simulation, JobID: id,
		Status: "queued", Outcome: "inconclusive", TestIDs: append([]string(nil), request.TestIDs...),
	}}
	s.addEvent(j, "job_queued", "")
	s.jobs[id] = j
	s.keys[request.IdempotencyKey] = submission{digest: digest, jobID: id}
	s.queue <- id // At most MaxJobs accepted jobs; the queue has the same bound.
	return protocol.Submission{ProtocolVersion: protocol.Version, Mode: protocol.Simulation, JobID: id}, nil
}

func (s *Service) Result(id string) (protocol.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return protocol.Result{}, ErrNotFound
	}
	result := j.result
	result.TestIDs = append([]string(nil), result.TestIDs...)
	return result, nil
}

func (s *Service) Events(id string, after int) (protocol.EventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return protocol.EventPage{}, ErrNotFound
	}
	page := protocol.EventPage{
		ProtocolVersion: protocol.Version, Mode: protocol.Simulation,
		Events: []protocol.Event{}, NextSeq: len(j.events), Terminal: terminal(j.result.Status),
	}
	for _, event := range j.events {
		if event.Seq > after {
			page.Events = append(page.Events, event)
		}
	}
	return page, nil
}

// Close stops the worker and settles queued simulations; it is safe to repeat.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if !terminal(j.result.Status) {
			j.result.Status = "cancelled"
			j.result.Outcome = "inconclusive"
			s.addEvent(j, "job_finished", "")
		}
	}
}

func (s *Service) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case id := <-s.queue:
			if s.ctx.Err() != nil {
				return
			}
			s.run(id)
		}
	}
}

func (s *Service) run(id string) {
	s.mu.Lock()
	j := s.jobs[id]
	j.result.Status = "running"
	ids := append([]string(nil), j.result.TestIDs...)
	s.addEvent(j, "attempt_started", "")
	s.mu.Unlock()
	err := fixture.Run(s.ctx, ids, func(kind, testID string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		j.result.Completed++
		if kind == "case_failed" {
			j.result.Failed++
		}
		s.addEvent(j, kind, testID)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	j.result.Status = "completed"
	j.result.Outcome = "passed"
	if err != nil {
		j.result.Status = "cancelled"
		j.result.Outcome = "inconclusive"
	} else if j.result.Failed > 0 {
		j.result.Outcome = "failed"
	}
	s.addEvent(j, "job_finished", "")
}

// addEvent is called only while mu is held or before a job is published.
func (s *Service) addEvent(j *job, kind, testID string) {
	j.events = append(j.events, protocol.Event{
		JobID: j.result.JobID, Seq: len(j.events) + 1, Kind: kind, TestID: testID,
	})
}

func terminal(status string) bool {
	return status == "completed" || status == "cancelled"
}
