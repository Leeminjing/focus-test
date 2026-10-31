package evidence

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Entry is a logical retained record. Rewrite renumbers only journal records;
// public per-job event sequences remain inside Value and are never changed.
type Entry struct {
	Kind  string
	Value any
}

func (j *Journal) Size() int64 { j.mu.Lock(); defer j.mu.Unlock(); return j.size }

func (j *Journal) Rewrite(entries []Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failed != nil {
		return j.failed
	}
	name := filepath.Join(j.directory, ".pending-"+rand.Text())
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		j.failed = err
		return err
	}
	defer os.Remove(name)
	var size int64
	for i, e := range entries {
		data, err := json.Marshal(e.Value)
		if err != nil {
			f.Close()
			return err
		}
		record := Record{Version: 1, Seq: i + 1, Kind: e.Kind, Data: data}
		record.Checksum = checksum(record)
		line, err := json.Marshal(record)
		if err != nil {
			f.Close()
			return err
		}
		line = append(line, '\n')
		if len(line) > MaxRecord || size+int64(len(line)) > MaxJournal {
			f.Close()
			return errors.New("retained checkpoint exceeds evidence quota")
		}
		n, err := f.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
		if err != nil {
			f.Close()
			j.failed = err
			return err
		}
		size += int64(n)
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		j.failed = err
		return err
	}
	// Close the old handle for Windows replacement while holding the same OS
	// state lock. A crash before rename leaves the old complete journal intact;
	// after rename, recovery sees only the new complete checksummed checkpoint.
	if err := j.file.Close(); err != nil {
		j.failed = err
		return err
	}
	j.file = nil
	if err := os.Rename(name, filepath.Join(j.directory, "events.jsonl")); err != nil {
		j.failed = err
		return err
	}
	j.file, err = os.OpenFile(filepath.Join(j.directory, "events.jsonl"), os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		j.failed = err
		return err
	}
	j.seq = len(entries)
	j.size = size
	return nil
}
