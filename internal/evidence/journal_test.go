package evidence_test

import (
	"os"
	"path/filepath"
	"testing"

	"focus-test-runtime/internal/evidence"
)

func TestDurabilityLockAndCorruption(t *testing.T) {
	dir := t.TempDir()
	j, _, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := evidence.Open(dir); err == nil {
		t.Fatal("second runtime acquired the same state")
	}
	if err := j.Append("example", map[string]string{"status": "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, records, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Kind != "example" {
		t.Fatalf("lost record: %+v", records)
	}
	reopened.Close()
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"partial":`)
	f.Close()
	if _, _, err := evidence.Open(dir); err == nil {
		t.Fatal("truncated journal was silently accepted")
	}
}
