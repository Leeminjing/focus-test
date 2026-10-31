package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// ExecutionRequest is separate from FixtureRequest. Its identity describes
// required execution inputs, not proof that those inputs have been executed.
type ExecutionRequest struct {
	ProtocolVersion  int         `json:"protocol_version"`
	IdempotencyKey   string      `json:"idempotency_key"`
	CallerRef        string      `json:"caller_ref"`
	TrustDomain      string      `json:"trust_domain"`
	SnapshotID       string      `json:"snapshot_id"`
	Plan             Plan        `json:"plan"`
	Environment      Environment `json:"environment"`
	Policy           Policy      `json:"policy"`
	Resources        Resources   `json:"resources"`
	RequestTimeoutMS int64       `json:"request_timeout_ms"`
	ReusePolicy      string      `json:"reuse_policy"`
}

type Plan struct {
	Adapter string   `json:"adapter"`
	TestIDs []string `json:"test_ids"`
	Args    []string `json:"args"`
	Workers int      `json:"workers"`
	Seed    int64    `json:"seed"`
}

type Environment struct {
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	RunnerDigest string            `json:"runner_digest"`
	Dependencies map[string]string `json:"dependencies"`
	Variables    map[string]string `json:"variables"`
	Data         map[string]string `json:"data"`
}

type Policy struct {
	ExecutionTimeoutMS int64  `json:"execution_timeout_ms"`
	Isolation          string `json:"isolation"`
	Network            string `json:"network"`
}

type Resources struct {
	CPUUnits  int      `json:"cpu_units"`
	MemoryMiB int      `json:"memory_mib"`
	Exclusive []string `json:"exclusive"`
}

// Binding carries execution identities without conflating a request, shared
// scheduled work, and a concrete process attempt.
type Binding struct {
	RequestID string `json:"request_id"`
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id,omitempty"`
}

func ValidDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	data, err := hex.DecodeString(value[7:])
	return err == nil && hex.EncodeToString(data) == value[7:]
}

func (r ExecutionRequest) Validate() error {
	if r.ProtocolVersion != Version {
		return fmt.Errorf("protocol_version must be %d", Version)
	}
	for _, value := range []string{r.IdempotencyKey, r.CallerRef, r.TrustDomain} {
		if len(strings.TrimSpace(value)) == 0 || len(value) > 256 {
			return errors.New("idempotency_key, caller_ref and trust_domain must contain 1..256 bytes")
		}
	}
	if !ValidDigest(r.SnapshotID) || !ValidDigest(r.Environment.RunnerDigest) {
		return errors.New("snapshot_id and environment.runner_digest must be SHA-256 identities")
	}
	if r.Environment.OS == "" || r.Environment.Arch == "" {
		return errors.New("environment OS and arch are required")
	}
	for _, inputs := range []map[string]string{r.Environment.Dependencies, r.Environment.Variables, r.Environment.Data} {
		for name, digest := range inputs {
			if name == "" || !ValidDigest(digest) {
				return errors.New("environment inputs require named SHA-256 identities; do not send secret values")
			}
		}
	}
	if r.Plan.Adapter != "node-v1" && r.Plan.Adapter != "node-sealed-v1" && r.Plan.Adapter != "pytest-v1" && r.Plan.Adapter != "fixture-v1" {
		return errors.New("unsupported versioned adapter")
	}
	if len(r.Plan.TestIDs) == 0 || len(r.Plan.TestIDs) > 10000 || r.Plan.Workers < 1 || r.Plan.Workers > 256 {
		return errors.New("plan requires 1..10000 test IDs and 1..256 workers")
	}
	seen := make(map[string]bool)
	for _, id := range r.Plan.TestIDs {
		if id == "" || len(id) > 4096 || seen[id] || strings.ContainsRune(id, '\x00') {
			return errors.New("test IDs must be nonempty, unique and bounded")
		}
		if r.Plan.Adapter != "fixture-v1" {
			file := strings.Split(id, "::")[0]
			if !fs.ValidPath(file) || file == "." || strings.ContainsAny(file, "\\:\x00*?[]") || strings.HasPrefix(file, "-") {
				return errors.New("test IDs must use explicit relative slash paths")
			}
			if (r.Plan.Adapter == "node-v1" || r.Plan.Adapter == "node-sealed-v1") && strings.Contains(id, "::") {
				return errors.New("node-v1 selects explicit test files")
			}
		}
		seen[id] = true
	}
	for _, arg := range r.Plan.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, '\x00') {
			return errors.New("invalid plan argument")
		}
	}
	if r.RequestTimeoutMS < 1 || r.RequestTimeoutMS > 86400000 || r.Policy.ExecutionTimeoutMS < 1 || r.Policy.ExecutionTimeoutMS > 86400000 {
		return errors.New("timeouts must be between 1 and 86400000 milliseconds")
	}
	if r.Policy.Isolation != "local-process" && r.Policy.Isolation != "focus-broker" {
		return errors.New("unsupported isolation policy")
	}
	if r.Policy.Network != "declared" && r.Policy.Network != "denied" {
		return errors.New("network policy must be declared or denied")
	}
	if r.Resources.CPUUnits < 1 || r.Resources.MemoryMiB < 1 || r.Resources.CPUUnits > 4096 || r.Resources.MemoryMiB > 1048576 || r.Plan.Workers > r.Resources.CPUUnits {
		return errors.New("invalid resource budget; workers cannot exceed CPU admission units")
	}
	seen = make(map[string]bool)
	for _, key := range r.Resources.Exclusive {
		if key == "" || len(key) > 256 || seen[key] {
			return errors.New("exclusive resource keys must be nonempty and unique")
		}
		seen[key] = true
	}
	if r.ReusePolicy != "none" && r.ReusePolicy != "inflight" {
		return errors.New("reuse_policy must be none or inflight; persistent reuse is not yet supported")
	}
	return nil
}

// ExecutionKey excludes caller identity and request waiting deadlines. Ordered
// scope and argv remain ordered; encoding/json sorts string map keys. Empty
// maps/slices normalize to empty collections so nil and empty cannot diverge.
func (r ExecutionRequest) ExecutionKey() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	r.Plan.Args = append([]string{}, r.Plan.Args...)
	r.Resources.Exclusive = append([]string{}, r.Resources.Exclusive...)
	r.Environment.Dependencies = nonnil(r.Environment.Dependencies)
	r.Environment.Variables = nonnil(r.Environment.Variables)
	r.Environment.Data = nonnil(r.Environment.Data)
	key := struct {
		Schema      int         `json:"schema"`
		TrustDomain string      `json:"trust_domain"`
		SnapshotID  string      `json:"snapshot_id"`
		Plan        Plan        `json:"plan"`
		Environment Environment `json:"environment"`
		Policy      Policy      `json:"policy"`
		Resources   Resources   `json:"resources"`
	}{Version, r.TrustDomain, r.SnapshotID, r.Plan, r.Environment, r.Policy, r.Resources}
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func nonnil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
