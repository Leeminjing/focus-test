package evidence_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"focus-test-runtime/internal/evidence"
)

func TestRewriteKeepsLockAndCompleteRecovery(t *testing.T) {
	dir := t.TempDir()
	j, _, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append("old", "discarded"); err != nil {
		t.Fatal(err)
	}
	if err := j.Rewrite([]evidence.Entry{{Kind: "retained", Value: map[string]int{"seq": 900}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := evidence.Open(dir); err == nil {
		t.Fatal("rewrite released owner lock")
	}
	if err := j.Append("next", "new"); err != nil {
		t.Fatal(err)
	}
	j.Close()
	// A crash before replacement leaves an unrelated partial pending file.
	if err := os.WriteFile(filepath.Join(dir, ".pending-crash"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, records, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := os.Stat(filepath.Join(dir, ".pending-crash")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned checkpoint was not reclaimed")
	}
	if len(records) != 2 || records[0].Kind != "retained" || records[1].Seq != 2 {
		t.Fatalf("bad checkpoint recovery: %+v", records)
	}
}

func TestReservedCapacityStillPermitsSettlement(t *testing.T) {
	j, _, err := evidence.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	rejected := false
	for i := 0; i < 10; i++ {
		err := j.AppendReserved("event", "payload", evidence.MaxJournal-512)
		if errors.Is(err, evidence.ErrCapacity) {
			rejected = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !rejected {
		t.Fatal("reserve did not apply backpressure")
	}
	if err := j.Append("settled", map[string]bool{"cleanup_confirmed": true}); err != nil {
		t.Fatal("capacity poisoned settlement", err)
	}
}

func TestInvalidCheckpointLeavesOriginalJournalIntact(t *testing.T) {
	dir := t.TempDir()
	j, _, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append("original", "valid"); err != nil {
		t.Fatal(err)
	}
	if err := j.Rewrite([]evidence.Entry{{Kind: "oversize", Value: strings.Repeat("x", evidence.MaxRecord)}}); err == nil {
		t.Fatal("oversized checkpoint accepted")
	}
	if err := j.Append("after", "valid"); err != nil {
		t.Fatal(err)
	}
	j.Close()
	restored, records, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if len(records) != 2 || records[0].Kind != "original" || records[1].Kind != "after" {
		t.Fatal("failed checkpoint replaced durable evidence")
	}
}
