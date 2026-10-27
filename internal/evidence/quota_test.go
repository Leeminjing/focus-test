package evidence_test

import (
	"strings"
	"testing"

	"focus-test-runtime/internal/evidence"
)

func TestQuotaFailureStopsFurtherPublication(t *testing.T) {
	j, _, err := evidence.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.Append("oversized", strings.Repeat("x", evidence.MaxRecord)); err == nil {
		t.Fatal("oversized record accepted")
	}
	if err := j.Append("pretend-success", map[string]bool{"passed": true}); err == nil {
		t.Fatal("poisoned journal published a later success")
	}
}
