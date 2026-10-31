package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Claim prevents two independent state owners from collecting the same store.
// CLI sealing remains allowed and participates in the operation OS lock.
func (s *Store) Claim(owner string) error {
	lock, err := s.operationLock(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	owner, err = filepath.Abs(owner)
	if err != nil {
		return err
	}
	owner, err = filepath.EvalSymlinks(owner)
	if err != nil {
		return err
	}
	if existing, err := s.root.ReadFile("gc-owner"); err == nil && !bytes.Equal(existing, []byte(owner)) {
		return errors.New("snapshot store is owned by another state directory")
	}
	return s.put("gc-owner", []byte(owner))
}

type Retention struct {
	Snapshots  map[string]bool
	Workspaces map[string]bool
	Before     time.Time
}

type Collection struct{ Manifests, Objects, Workspaces int }

func (s *Store) Collect(ctx context.Context, keep Retention) (Collection, error) {
	lock, err := s.operationLock(ctx)
	if err != nil {
		return Collection{}, err
	}
	defer lock.Close()
	result := Collection{}
	objects := map[string]bool{}
	for id := range keep.Snapshots {
		sealed, err := s.load(id)
		if err != nil {
			return result, err
		}
		for _, input := range sealed.Manifest.Entries {
			if input.Kind == "file" {
				objects[strings.TrimPrefix(input.Digest, "sha256:")] = true
			}
		}
	}
	manifests, err := s.entries("manifests")
	if err != nil {
		return result, err
	}
	for _, entry := range manifests {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		info, err := entry.Info()
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() {
			return result, ErrCorrupt
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".pending-") {
			if info.ModTime().Before(keep.Before) {
				if err := s.root.Remove("manifests/" + name); err != nil {
					return result, err
				}
			}
			continue
		}
		id := "sha256:" + strings.TrimSuffix(name, ".json")
		if !strings.HasSuffix(name, ".json") {
			return result, ErrCorrupt
		}
		if keep.Snapshots[id] || !info.ModTime().Before(keep.Before) {
			sealed, err := s.load(id)
			if err != nil {
				return result, err
			}
			for _, input := range sealed.Manifest.Entries {
				if input.Kind == "file" {
					objects[strings.TrimPrefix(input.Digest, "sha256:")] = true
				}
			}
		} else {
			if _, err := digestKey(id); err != nil {
				return result, err
			}
			if err := s.root.Remove("manifests/" + name); err != nil {
				return result, err
			}
			result.Manifests++
		}
	}
	entries, err := s.entries("objects")
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		info, err := entry.Info()
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() {
			return result, ErrCorrupt
		}
		if !objects[entry.Name()] && info.ModTime().Before(keep.Before) {
			if _, err := digestKey("sha256:" + entry.Name()); err != nil && !strings.HasPrefix(entry.Name(), ".pending-") {
				return result, err
			}
			if err := s.root.Remove("objects/" + entry.Name()); err != nil {
				return result, err
			}
			result.Objects++
		}
	}
	entries, err = s.entries("work")
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		name := path.Join("work", entry.Name())
		directory := filepath.Join(s.path, filepath.FromSlash(name))
		if keep.Workspaces[directory] {
			continue
		}
		info, err := s.root.Lstat(name)
		if err != nil {
			return result, err
		}
		if info.ModTime().Before(keep.Before) {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return result, errors.New("unexpected workspace link or file")
			}
			// The only recursive targets are direct work children obtained through
			// the rooted store. Never delete a caller-provided absolute directory.
			if err := fs.WalkDir(s.root.FS(), name, func(p string, d fs.DirEntry, e error) error {
				if e != nil {
					return e
				}
				info, e := s.root.Lstat(p)
				if e != nil {
					return e
				}
				if info.Mode()&os.ModeType == 0 {
					return s.root.Chmod(p, 0600)
				}
				if info.Mode()&os.ModeType == os.ModeDir {
					return s.root.Chmod(p, 0700)
				}
				return nil
			}); err != nil {
				return result, err
			}
			if err := s.root.RemoveAll(name); err != nil {
				return result, err
			}
			result.Workspaces++
		}
	}
	return result, nil
}

func (s *Store) entries(name string) ([]os.DirEntry, error) {
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}
