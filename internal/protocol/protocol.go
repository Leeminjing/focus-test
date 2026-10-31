// Package protocol defines the bootstrap simulation protocol. Simulation results
// are diagnostic examples, never evidence that application tests passed.
package protocol

import (
	"errors"
	"fmt"
	"strings"
)

const Version = 1
const Simulation = "simulation"

// FixtureRequest deliberately has no shell command, workspace, or execution
// permission. Real execution will use a separate, snapshot-bound contract.
type FixtureRequest struct {
	ProtocolVersion int      `json:"protocol_version"`
	IdempotencyKey  string   `json:"idempotency_key"`
	TestIDs         []string `json:"test_ids"`
}

func (r FixtureRequest) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol_version must be %d", Version)
	}
	if len(r.IdempotencyKey) == 0 || len(r.IdempotencyKey) > 128 {
		return errors.New("idempotency_key must contain 1..128 bytes")
	}
	if len(r.TestIDs) == 0 || len(r.TestIDs) > 32 {
		return errors.New("test_ids must contain 1..32 fixture IDs")
	}
	seen := make(map[string]bool)
	for _, id := range r.TestIDs {
		switch id {
		case "fixture.pass", "fixture.fail", "fixture.slow":
		default:
			return fmt.Errorf("unsupported fixture ID %q", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate fixture ID %q", id)
		}
		seen[id] = true
	}
	return nil
}

type Submission struct {
	ProtocolVersion int    `json:"protocol_version"`
	Mode            string `json:"mode"`
	JobID           string `json:"job_id"`
}

type Event struct {
	JobID   string `json:"job_id"`
	Seq     int    `json:"seq"`
	Kind    string `json:"kind"`
	TestID  string `json:"test_id,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Message string `json:"message,omitempty"`
}

type Result struct {
	ProtocolVersion int      `json:"protocol_version"`
	Mode            string   `json:"mode"`
	JobID           string   `json:"job_id"`
	Status          string   `json:"status"`
	Outcome         string   `json:"outcome"`
	TestIDs         []string `json:"test_ids"`
	Completed       int      `json:"completed"`
	Failed          int      `json:"failed"`
}

type EventPage struct {
	ProtocolVersion int     `json:"protocol_version"`
	Mode            string  `json:"mode"`
	Events          []Event `json:"events"`
	NextSeq         int     `json:"next_seq"`
	Terminal        bool    `json:"terminal"`
	HasMore         bool    `json:"has_more"`
}

const DefaultEventLimit = 128
const MaxEventLimit = 256
const MaxEventPageBytes = 256 << 10

type EventQuery struct {
	AfterSeq int
	Limit    int
}

type ReferenceRequest struct {
	ReferenceID string `json:"reference_id"`
}

func (r ReferenceRequest) Validate() error {
	if len(r.ReferenceID) > 256 || strings.TrimSpace(r.ReferenceID) == "" || strings.ContainsRune(r.ReferenceID, '\x00') {
		return errors.New("reference_id must contain 1..256 nonempty bytes")
	}
	return nil
}

func (q EventQuery) Validate() error {
	if q.AfterSeq < 0 || q.Limit < 1 || q.Limit > MaxEventLimit {
		return errors.New("event cursor must be nonnegative and limit must be 1..256")
	}
	return nil
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
