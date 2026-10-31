package snapshot_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"focus-test-runtime/internal/snapshot"
)

func TestCollectionProtectsInputsAndSharedObjects(t *testing.T) {
	s, err := snapshot.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := t.TempDir()
	seal := func(name string) snapshot.Sealed {
		t.Helper()
		for file, value := range map[string]string{"shared.cjs": "same", "unique.cjs": name} {
			if err := os.WriteFile(filepath.Join(source, file), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
		sealed, err := s.Seal(context.Background(), source, nil)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	a := seal("A")
	wa, err := s.Materialize(context.Background(), a.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	b := seal("B")
	wb, err := s.Materialize(context.Background(), b.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	collected, err := s.Collect(context.Background(), snapshot.Retention{Snapshots: map[string]bool{a.SnapshotID: true}, Workspaces: map[string]bool{wa: true}, Before: time.Now().Add(time.Hour)})
	if err != nil || collected.Manifests != 1 || collected.Objects != 1 || collected.Workspaces != 1 {
		t.Fatalf("bad sweep: %+v %v", collected, err)
	}
	if _, err := os.Stat(wb); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreferenced workspace remained")
	}
	if err := s.VerifyWorkspace(context.Background(), a.SnapshotID, wa); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Materialize(context.Background(), a.SnapshotID); err != nil {
		t.Fatal("shared object was deleted", err)
	}
	if _, err := s.Load(b.SnapshotID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired manifest remained")
	}
}
