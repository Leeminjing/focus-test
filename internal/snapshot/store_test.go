package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, t.TempDir()
}

func writeInput(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSealDirtyInputsAndMaterializeIndependentCopies(t *testing.T) {
	s, source := testStore(t)
	writeInput(t, source, "source.go", "original\r\n")
	writeInput(t, source, "untracked.txt", "untracked")
	writeInput(t, source, ".gitignore", "untracked.txt\n")
	if err := os.Mkdir(filepath.Join(source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	first, err := s.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Seal(context.Background(), source, nil)
	if err != nil || first.SnapshotID != again.SnapshotID {
		t.Fatalf("stable input identity changed: %v", err)
	}
	writeInput(t, source, "source.go", "changed\r\n")
	second, err := s.Seal(context.Background(), source, nil)
	if err != nil || first.SnapshotID == second.SnapshotID {
		t.Fatalf("dirty bytes reused the old identity: %v", err)
	}
	a, err := s.Materialize(context.Background(), first.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Materialize(context.Background(), first.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	writeInput(t, a, "source.go", "attempt A changed this")
	data, err := os.ReadFile(filepath.Join(b, "source.go"))
	if err != nil || string(data) != "original\r\n" {
		t.Fatalf("attempts share mutable content: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(b, "untracked.txt")); err != nil || string(data) != "untracked" {
		t.Fatal("Git-ignored input was lost")
	}
	if info, err := os.Stat(filepath.Join(b, "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty input directory was lost")
	}
}

func TestSelfStoreExcludedAndSourceNotPathDependent(t *testing.T) {
	source := t.TempDir()
	writeInput(t, source, "test.txt", "same")
	s, err := Open(filepath.Join(source, ".focus-test", "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	writeInput(t, other, "test.txt", "same")
	second, err := s.Seal(context.Background(), other, []string{".focus-test/snapshots"})
	if err != nil || first.SnapshotID != second.SnapshotID {
		t.Fatalf("same declared inputs at different paths diverged: %v", err)
	}
	for _, e := range first.Manifest.Entries {
		if strings.HasPrefix(e.Path, ".focus-test") {
			t.Fatal("content store included itself")
		}
	}
}

func TestCorruptionAndTraversalFailClosed(t *testing.T) {
	s, source := testStore(t)
	writeInput(t, source, "file.txt", "original")
	sealed, err := s.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := digestKey(sealed.Manifest.Entries[0].Digest)
	if err := s.root.WriteFile("objects/"+key, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Materialize(context.Background(), sealed.SnapshotID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt object accepted: %v", err)
	}
	for _, id := range []string{"../../outside", "sha256:" + strings.Repeat("A", 64), "sha256:../evil"} {
		if _, err := s.Load(id); err == nil {
			t.Fatalf("invalid identity accepted: %s", id)
		}
	}
	if _, err := s.Seal(context.Background(), source, []string{"../outside"}); err == nil {
		t.Fatal("out-of-root exclusion accepted")
	}
}

func TestStaleReadAndCancellation(t *testing.T) {
	s, source := testStore(t)
	writeInput(t, source, "file.txt", "before")
	r, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before, _ := r.Lstat("file.txt")
	writeInput(t, source, "file.txt", "after change")
	if _, err := readStable(context.Background(), r, "file.txt", before, 1024); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("stale input observation was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Seal(ctx, source, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled seal accepted: %v", err)
	}
}

func TestLinksAndModes(t *testing.T) {
	s, source := testStore(t)
	writeInput(t, source, "run.sh", "echo test\n")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(source, "run.sh"), 0751); err != nil {
			t.Fatal(err)
		}
		sealed, err := s.Seal(context.Background(), source, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir, err := s.Materialize(context.Background(), sealed.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(filepath.Join(dir, "run.sh"))
		if info.Mode().Perm() != 0751 {
			t.Fatal("executable mode was changed")
		}
	}
	if err := os.Symlink(filepath.Join(source, "run.sh"), filepath.Join(source, "link")); err != nil {
		t.Skipf("host does not allow symlink creation: %v", err)
	}
	if _, err := s.Seal(context.Background(), source, nil); err == nil {
		t.Fatal("symlink was accepted")
	}
}

func TestTimeMetadataAloneDoesNotChangeContentIdentity(t *testing.T) {
	s, source := testStore(t)
	writeInput(t, source, "file.txt", "same bytes")
	a, err := s.Seal(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(source, "file.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	b, err := s.Seal(context.Background(), source, nil)
	if err != nil || a.SnapshotID != b.SnapshotID {
		t.Fatalf("mtime incorrectly changes execution inputs: %v", err)
	}
}
