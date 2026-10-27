package runtime

import (
	"errors"
	"io"
	"os"
)

type LogPage struct {
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Data       string `json:"data"`
}

func (s *Service) ReadLog(requestID, stream string, offset int64) (LogPage, error) {
	if (stream != "stdout" && stream != "stderr") || offset < 0 {
		return LogPage{}, errors.New("invalid log stream or offset")
	}
	s.mu.Lock()
	r := s.requests[requestID]
	if r == nil {
		s.mu.Unlock()
		return LogPage{}, ErrNotFound
	}
	directory := s.jobs[r.JobID].Result.Workspace
	s.mu.Unlock()
	if directory == "" {
		return LogPage{}, ErrNotFound
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return LogPage{}, err
	}
	defer root.Close()
	name := ".focus-test/" + stream + ".log"
	info, err := root.Lstat(name)
	if err != nil {
		return LogPage{}, err
	}
	if !info.Mode().IsRegular() {
		return LogPage{}, errors.New("log is not a regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return LogPage{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return LogPage{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return LogPage{}, err
	}
	return LogPage{Offset: offset, NextOffset: offset + int64(len(data)), Data: string(data)}, nil
}
