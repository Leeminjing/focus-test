package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"focus-test-runtime/internal/evidence"
	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/snapshot"
)

type RetentionPolicy struct {
	MaxAge     time.Duration
	MinimumAge time.Duration
	Interval   time.Duration
	InputGrace time.Duration
}

func (p *RetentionPolicy) defaults() error {
	if p.MaxAge == 0 {
		p.MaxAge = 24 * time.Hour
	}
	if p.MinimumAge == 0 {
		p.MinimumAge = time.Minute
	}
	if p.Interval == 0 {
		p.Interval = time.Minute
	}
	if p.InputGrace == 0 {
		p.InputGrace = 15 * time.Minute
	}
	if p.MaxAge < 0 || p.MinimumAge < 0 || p.Interval < 0 || p.InputGrace < 0 || p.MinimumAge > p.MaxAge {
		return errors.New("invalid retention policy")
	}
	return nil
}

type Collection struct {
	Jobs      int                 `json:"jobs"`
	Requests  int                 `json:"requests"`
	Snapshots snapshot.Collection `json:"snapshots"`
}

func (s *Service) Collect() (Collection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Collection{}, ErrClosed
	}
	return s.collectLocked(time.Now(), false, nil)
}

func (s *Service) Reference(jobID, reference string, pin bool) error {
	if err := (protocol.ReferenceRequest{ReferenceID: reference}).Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.fatal != nil {
		return s.fatal
	}
	j := s.jobs[jobID]
	if j == nil {
		return ErrNotFound
	}
	if j.References[reference] == pin {
		return nil
	}
	if pin && len(j.References) >= 1024 {
		return ErrCapacity
	}
	status := "released"
	if pin {
		status = "pinned"
	}
	return s.commit(change{Kind: "reference", JobID: jobID, Reference: reference, Status: status})
}

func (s *Service) collectLocked(now time.Time, pressure bool, pending *change) (Collection, error) {
	result := Collection{}
	if s.fatal != nil {
		return result, s.fatal
	}
	victims := map[string]bool{}
	age := s.config.Retention.MaxAge
	if pressure {
		age = s.config.Retention.MinimumAge
	}
	for id, j := range s.jobs {
		if pending != nil && pending.JobID == id {
			continue
		}
		if terminal(j.Result.Status) && j.Result.CleanupStatus != "unknown" && len(j.References) == 0 && !j.Finished.IsZero() && !now.Before(j.Finished.Add(age)) {
			victims[id] = true
		}
	}
	// Checkpoint first, then forget metadata, then sweep files. A crash can leave
	// garbage behind, but cannot delete evidence still present in durable state.
	compact := s.journal != nil && s.journal.Size() > evidence.MaxJournal*3/4 && (s.lastCompactSize == 0 || s.journal.Size() >= s.lastCompactSize+(4<<20))
	if pending != nil && pending.Kind == "reference" && pending.Status == "released" && s.journal != nil && s.journal.Size() > evidence.MaxJournal-(2<<20) {
		compact = true
	}
	if pressure && len(victims) == 0 && !compact {
		return result, nil
	}
	if len(victims) > 0 || compact {
		entries, err := s.checkpoint(victims)
		if err != nil {
			return result, err
		}
		if s.journal != nil {
			if err := s.journal.Rewrite(entries); err != nil {
				s.fatal = fmt.Errorf("evidence_unavailable: %w", err)
				s.cancel()
				return result, s.fatal
			}
			s.lastCompactSize = s.journal.Size()
		}
		for id := range victims {
			delete(s.jobs, id)
			result.Jobs++
		}
		for id, r := range s.requests {
			if victims[r.JobID] {
				delete(s.keys, r.Scope)
				delete(s.requests, id)
				result.Requests++
			}
		}
		queue := s.queue[:0]
		for _, id := range s.queue {
			if !victims[id] {
				queue = append(queue, id)
			}
		}
		s.queue = queue
	}
	if pending == nil && s.config.Snapshots != nil && s.journal != nil && s.ctx.Err() == nil {
		keep := snapshot.Retention{Snapshots: map[string]bool{}, Workspaces: map[string]bool{}, Before: now.Add(-s.config.Retention.InputGrace)}
		for _, j := range s.jobs {
			if !j.Legacy {
				keep.Snapshots[j.Spec.SnapshotID] = true
			}
			if j.Result.Workspace != "" {
				keep.Workspaces[j.Result.Workspace] = true
			}
		}
		var err error
		result.Snapshots, err = s.config.Snapshots.Collect(s.ctx, keep)
		if err != nil {
			s.maintenanceError = err.Error()
			return result, err
		}
		s.maintenanceError = ""
	}
	return result, nil
}

func (s *Service) RetentionStatus() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	bytes := int64(0)
	if s.journal != nil {
		bytes = s.journal.Size()
	}
	return map[string]any{"retained_jobs": len(s.jobs), "retained_requests": len(s.requests), "journal_bytes": bytes, "last_collection_error": s.maintenanceError}
}

type restoredEvents struct {
	JobID  string           `json:"job_id"`
	Events []protocol.Event `json:"events"`
}

func (s *Service) checkpoint(victims map[string]bool) ([]evidence.Entry, error) {
	entries := []evidence.Entry{}
	ids := []string{}
	for id := range s.jobs {
		if !victims[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := s.jobs[id]
		copy := *j
		copy.Events = nil
		entries = append(entries, evidence.Entry{Kind: "retained_job", Value: copy})
		for offset := 0; offset < len(j.Events); {
			chunk := restoredEvents{JobID: id}
			size := 1024
			for offset < len(j.Events) {
				encoded, err := json.Marshal(j.Events[offset])
				if err != nil {
					return nil, err
				}
				if size+len(encoded) > protocol.MaxEventPageBytes {
					break
				}
				size += len(encoded) + 1
				chunk.Events = append(chunk.Events, j.Events[offset])
				offset++
			}
			if len(chunk.Events) == 0 {
				return nil, errors.New("retained event exceeds checkpoint record budget")
			}
			entries = append(entries, evidence.Entry{Kind: "retained_events", Value: chunk})
		}
	}
	ids = nil
	for id, r := range s.requests {
		if !victims[r.JobID] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		entries = append(entries, evidence.Entry{Kind: "retained_request", Value: s.requests[id]})
	}
	return entries, nil
}

func (s *Service) restore(record evidence.Record) error {
	switch record.Kind {
	case "retained_job":
		var j job
		if json.Unmarshal(record.Data, &j) != nil || j.ID == "" || j.ID != j.Result.JobID || s.jobs[j.ID] != nil || len(j.Events) != 0 {
			return errors.New("invalid retained job")
		}
		s.jobs[j.ID] = &j
	case "retained_events":
		var e restoredEvents
		if json.Unmarshal(record.Data, &e) != nil || s.jobs[e.JobID] == nil {
			return errors.New("invalid retained events")
		}
		j := s.jobs[e.JobID]
		for _, event := range e.Events {
			if event.JobID != j.ID || event.Seq != len(j.Events)+1 {
				return errors.New("invalid retained event sequence")
			}
			j.Events = append(j.Events, event)
		}
	case "retained_request":
		var r requestState
		if json.Unmarshal(record.Data, &r) != nil || r.ID == "" || s.requests[r.ID] != nil || s.jobs[r.JobID] == nil {
			return errors.New("invalid retained request")
		}
		s.requests[r.ID] = &r
		s.keys[r.Scope] = r.ID
	default:
		return errors.New("unknown checkpoint record")
	}
	return nil
}
