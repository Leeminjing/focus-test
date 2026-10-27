// Package evidence owns checksummed, synchronous execution records.
package evidence

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const MaxRecord = 1 << 20
const MaxJournal = 64 << 20

type Record struct {
	Version  int             `json:"version"`
	Seq      int             `json:"seq"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
	Checksum string          `json:"checksum"`
}

type Journal struct {
	mu     sync.Mutex
	file   *os.File
	lock   *os.File
	seq    int
	size   int64
	failed error
}

// Open acquires a process-scoped OS lock before reading any execution records.
// Corrupt or truncated records fail closed; they are never reinterpreted as a
// successful run or silently replaced with an empty journal.
func Open(directory string) (*Journal, []Record, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, nil, err
	}
	lock, err := lockFile(filepath.Join(directory, "runtime.lock"))
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	j := &Journal{file: f, lock: lock}
	fail := func(err error) (*Journal, []Record, error) { j.Close(); return nil, nil, err }
	info, err := f.Stat()
	if err != nil || info.Size() > MaxJournal {
		return fail(errors.New("journal size unavailable or quota exceeded"))
	}
	j.size = info.Size()
	reader := bufio.NewReaderSize(f, 64<<10)
	var records []Record
	for {
		line, err := readRecord(reader)
		if err == io.EOF && len(line) == 0 {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("journal incomplete or oversized: %w", err))
		}
		var record Record
		if json.Unmarshal(line, &record) != nil || record.Version != 1 || record.Seq != j.seq+1 || record.Checksum != checksum(record) {
			return fail(errors.New("journal corrupt: sequence, version or checksum mismatch"))
		}
		j.seq = record.Seq
		records = append(records, record)
	}
	return j, records, nil
}

func readRecord(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > MaxRecord {
			return nil, errors.New("record quota exceeded")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func (j *Journal) Append(kind string, value any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failed != nil {
		return j.failed
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	record := Record{Version: 1, Seq: j.seq + 1, Kind: kind, Data: data}
	record.Checksum = checksum(record)
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxRecord || j.size+int64(len(encoded)) > MaxJournal {
		j.failed = errors.New("evidence quota exceeded; stop admission")
		return j.failed
	}
	n, err := j.file.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.failed = err
		return err
	}
	j.seq++
	j.size += int64(n)
	return nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	lockErr := j.lock.Close()
	j.file = nil
	j.failed = errors.New("journal closed")
	return errors.Join(err, lockErr)
}

func checksum(record Record) string {
	record.Checksum = ""
	encoded, _ := json.Marshal(record)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
