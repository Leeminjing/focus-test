// Package snapshot seals declared directory inputs into a verified content store.
// A verified copy records actual bytes; it is not a filesystem atomic snapshot.
package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"focus-test-runtime/internal/evidence"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

const Version = 1
const MaxFiles = 10000
const MaxBytes int64 = 256 << 20

var ErrInputChanged = errors.New("input_changed: source changed during sealing")
var ErrCorrupt = errors.New("snapshot_corrupt: content or manifest failed verification")

type Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
}

type Manifest struct {
	Version     int      `json:"version"`
	Platform    string   `json:"platform"`
	Consistency string   `json:"consistency"`
	Excludes    []string `json:"excludes"`
	Entries     []Entry  `json:"entries"`
}

type Sealed struct {
	SnapshotID string   `json:"snapshot_id"`
	Manifest   Manifest `json:"manifest"`
}

// Store's directory must be private to the runtime. Root guards path traversal;
// content hashes detect accidental edits. It is not a hostile-user filesystem.
type Store struct {
	root       *os.Root
	path       string
	operations chan struct{}
}

func Open(directory string) (*Store, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"objects", "manifests", "work"} {
		if err := r.MkdirAll(dir, 0700); err != nil {
			r.Close()
			return nil, err
		}
	}
	operations := make(chan struct{}, 1)
	operations <- struct{}{}
	return &Store{root: r, path: resolved, operations: operations}, nil
}

func (s *Store) Close() error { return s.root.Close() }

func (s *Store) Seal(ctx context.Context, source string, excludes []string) (Sealed, error) {
	lock, err := s.operationLock(ctx)
	if err != nil {
		return Sealed{}, err
	}
	defer lock.Close()
	abs, err := filepath.Abs(source)
	if err != nil {
		return Sealed{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Sealed{}, err
	}
	if info.Mode()&os.ModeType != os.ModeDir {
		return Sealed{}, errors.New("source must be an ordinary directory, not a link or special file")
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return Sealed{}, err
	}
	if within(s.path, abs) {
		return Sealed{}, errors.New("source cannot be inside the snapshot store")
	}
	if within(abs, s.path) {
		rel, _ := filepath.Rel(abs, s.path)
		excludes = append(append([]string(nil), excludes...), filepath.ToSlash(rel))
	}
	excludes, err = normalizeExcludes(excludes)
	if err != nil {
		return Sealed{}, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return Sealed{}, err
	}
	defer r.Close()
	first, err := s.scan(ctx, r, excludes, true)
	if err != nil {
		return Sealed{}, err
	}
	second, err := s.scan(ctx, r, excludes, false)
	if err != nil {
		return Sealed{}, fmt.Errorf("%w: %v", ErrInputChanged, err)
	}
	current, err := os.Stat(abs)
	if err != nil || !os.SameFile(info, current) || !reflect.DeepEqual(first, second) {
		return Sealed{}, ErrInputChanged
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		return Sealed{}, err
	}
	id := hash(encoded)
	if err := s.put("manifests/"+id+".json", encoded); err != nil {
		return Sealed{}, err
	}
	if err := s.root.Chtimes("manifests/"+id+".json", time.Now(), time.Now()); err != nil {
		return Sealed{}, err
	}
	return Sealed{SnapshotID: "sha256:" + id, Manifest: first}, nil
}

func (s *Store) scan(ctx context.Context, r *os.Root, excludes []string, save bool) (Manifest, error) {
	m := Manifest{Version: Version, Platform: runtime.GOOS, Consistency: "verified_copy", Excludes: excludes, Entries: []Entry{}}
	var total int64
	err := fs.WalkDir(r.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		for _, exclude := range excludes {
			if name == exclude || strings.HasPrefix(name, exclude+"/") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if !validPath(name) {
			return fmt.Errorf("unsupported input path %q", name)
		}
		info, err := r.Lstat(name)
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode&os.ModeType != 0 && mode&os.ModeType != os.ModeDir {
			return fmt.Errorf("unsupported link or special file %q", name)
		}
		entry := Entry{Path: name, Mode: uint32(mode.Perm()), Kind: "directory"}
		if mode.IsRegular() {
			entry.Kind = "file"
			data, err := readStable(ctx, r, name, info, MaxBytes-total)
			if err != nil {
				return err
			}
			entry.Size, entry.Digest = int64(len(data)), "sha256:"+hash(data)
			total += entry.Size
			if save {
				if err := s.put("objects/"+hash(data), data); err != nil {
					return err
				}
			}
		}
		m.Entries = append(m.Entries, entry)
		if len(m.Entries) > MaxFiles {
			return errors.New("snapshot exceeds entry limit")
		}
		return nil
	})
	// WalkDir's depth-first order is deterministic, but the manifest contract
	// specifies lexical order independently of the traversal implementation.
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, err
}

func readStable(ctx context.Context, r *os.Root, name string, before fs.FileInfo, limit int64) ([]byte, error) {
	if before.Size() > limit {
		return nil, errors.New("snapshot exceeds byte limit")
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, ErrInputChanged
	}
	var data bytes.Buffer
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := f.Read(buffer)
		if int64(data.Len()+n) > limit {
			return nil, errors.New("snapshot exceeds byte limit")
		}
		data.Write(buffer[:n])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		return nil, ErrInputChanged
	}
	return data.Bytes(), nil
}

func (s *Store) Load(id string) (Sealed, error) {
	lock, err := s.operationLock(context.Background())
	if err != nil {
		return Sealed{}, err
	}
	defer lock.Close()
	return s.load(id)
}

func (s *Store) load(id string) (Sealed, error) {
	key, err := digestKey(id)
	if err != nil {
		return Sealed{}, err
	}
	f, err := s.root.Open("manifests/" + key + ".json")
	if err != nil {
		return Sealed{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	f.Close()
	if readErr != nil || len(data) > 4<<20 || hash(data) != key {
		return Sealed{}, ErrCorrupt
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil || m.Version != Version || m.Platform != runtime.GOOS || m.Consistency != "verified_copy" || len(m.Entries) > MaxFiles {
		return Sealed{}, ErrCorrupt
	}
	last := ""
	var total int64
	dirs := map[string]bool{".": true}
	for _, entry := range m.Entries {
		if !validPath(entry.Path) || entry.Path <= last || !dirs[path.Dir(entry.Path)] || entry.Mode > 0777 || entry.Size < 0 {
			return Sealed{}, ErrCorrupt
		}
		last = entry.Path
		switch entry.Kind {
		case "directory":
			if entry.Size != 0 || entry.Digest != "" {
				return Sealed{}, ErrCorrupt
			}
			dirs[entry.Path] = true
		case "file":
			if _, err := digestKey(entry.Digest); err != nil {
				return Sealed{}, ErrCorrupt
			}
			total += entry.Size
			if total > MaxBytes {
				return Sealed{}, ErrCorrupt
			}
		default:
			return Sealed{}, ErrCorrupt
		}
	}
	return Sealed{SnapshotID: id, Manifest: m}, nil
}

// Materialize creates a private writable copy beneath the store. The returned
// directory never shares writable hard links with content objects or source.
func (s *Store) Materialize(ctx context.Context, id string) (string, error) {
	lock, err := s.operationLock(ctx)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	sealed, err := s.load(id)
	if err != nil {
		return "", err
	}
	name := "work/" + rand.Text()
	if err := s.root.Mkdir(name, 0700); err != nil {
		return "", err
	}
	r, err := s.root.OpenRoot(name)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		r.Close()
		if !success {
			_ = s.root.RemoveAll(name)
		}
	}()
	for _, entry := range sealed.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if entry.Kind == "directory" {
			if err := r.Mkdir(entry.Path, 0700); err != nil {
				return "", err
			}
			continue
		}
		key, _ := digestKey(entry.Digest)
		f, err := s.root.Open("objects/" + key)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, entry.Size+1))
		f.Close()
		if readErr != nil || int64(len(data)) != entry.Size || hash(data) != key {
			return "", ErrCorrupt
		}
		if err := r.WriteFile(entry.Path, data, 0600); err != nil {
			return "", err
		}
		if err := r.Chmod(entry.Path, fs.FileMode(entry.Mode)); err != nil {
			return "", err
		}
	}
	for i := len(sealed.Manifest.Entries) - 1; i >= 0; i-- {
		entry := sealed.Manifest.Entries[i]
		if entry.Kind == "directory" {
			if err := r.Chmod(entry.Path, fs.FileMode(entry.Mode)); err != nil {
				return "", err
			}
		}
	}
	success = true
	return filepath.Join(s.path, filepath.FromSlash(name)), nil
}

type operationGuard struct {
	file    *os.File
	release chan struct{}
}

func (g *operationGuard) Close() error {
	err := evidence.UnlockOperation(g.file)
	closeErr := g.file.Close()
	g.release <- struct{}{}
	return errors.Join(err, closeErr)
}

func (s *Store) operationLock(ctx context.Context) (*operationGuard, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.operations:
	}
	f, err := s.root.OpenFile("operations.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		s.operations <- struct{}{}
		return nil, err
	}
	if err := evidence.LockOperation(ctx, f); err != nil {
		f.Close()
		s.operations <- struct{}{}
		return nil, err
	}
	return &operationGuard{file: f, release: s.operations}, nil
}

// VerifyWorkspace checks the original declared inputs after execution. Generated
// outputs are permitted, but edits/deletions/links replacing inputs invalidate
// a passing result. The workspace remains inside the private content store.
func (s *Store) VerifyWorkspace(ctx context.Context, id, directory string) error {
	sealed, err := s.Load(id)
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, entry := range sealed.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := r.Lstat(entry.Path)
		if err != nil || uint32(info.Mode().Perm()) != entry.Mode {
			return ErrInputChanged
		}
		if entry.Kind == "directory" {
			if info.Mode()&os.ModeType != os.ModeDir {
				return ErrInputChanged
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return ErrInputChanged
		}
		data, err := readStable(ctx, r, entry.Path, info, MaxBytes)
		if err != nil || "sha256:"+hash(data) != entry.Digest {
			return ErrInputChanged
		}
	}
	return nil
}

// put writes a complete object before publication. Existing objects are always
// checked, rather than trusting a path named after a hash.
func (s *Store) put(name string, data []byte) error {
	if existing, err := s.root.ReadFile(name); err == nil {
		if !bytes.Equal(existing, data) {
			return ErrCorrupt
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	temp := path.Dir(name) + "/.pending-" + rand.Text()
	f, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(temp)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := s.root.Rename(temp, name); err != nil {
		if existing, readErr := s.root.ReadFile(name); readErr == nil && bytes.Equal(existing, data) {
			return nil
		}
		return err
	}
	return nil
}

func normalizeExcludes(values []string) ([]string, error) {
	seen := map[string]bool{".git": true, ".focus-test": true}
	for _, value := range values {
		if !validPath(value) {
			return nil, fmt.Errorf("exclude must be a relative slash path: %q", value)
		}
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validPath(value string) bool {
	return value != "." && fs.ValidPath(value) && filepath.IsLocal(value) && !strings.ContainsAny(value, "\\:\x00")
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func digestKey(value string) (string, error) {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return "", errors.New("expected sha256 followed by 64 lowercase hex digits")
	}
	key := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(key)
	if err != nil || hex.EncodeToString(decoded) != key {
		return "", errors.New("invalid content digest")
	}
	return key, nil
}

func hash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
