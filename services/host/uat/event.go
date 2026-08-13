package uat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Event struct {
	Sequence int             `json:"sequence"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

type EventStore struct {
	path string
	mu   sync.Mutex
}

func validatePath(path string) error {
	parent := filepath.Dir(path)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if parentInfo.Mode().Perm() != 0700 {
		return fmt.Errorf("parent directory %s must have 0700 permissions", parent)
	}

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("event file cannot be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("event file must be a regular file")
	}
	if info.Mode().Perm() != 0600 {
		return fmt.Errorf("event file %s must have 0600 permissions", path)
	}
	return nil
}

func NewEventStore(path string) (*EventStore, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &EventStore{path: path}, nil
}

func (s *EventStore) Append(eventType string, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Lock the file for cross-process safety
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Validate existing content and find last sequence
	decoder := json.NewDecoder(f)
	lastSeq := 0
	for {
		var evt Event
		if err := decoder.Decode(&evt); err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("malformed JSONL or invalid event: %w", err)
		}
		if evt.Sequence <= lastSeq {
			return 0, errors.New("non-monotonic sequence detected")
		}
		lastSeq = evt.Sequence
	}
	newSeq := lastSeq + 1

	// Append new event
	evt := Event{Sequence: newSeq, Type: eventType, Data: data}
	b, err := json.Marshal(evt)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return 0, err
	}

	// fsync
	if err := f.Sync(); err != nil {
		return 0, err
	}

	return newSeq, nil
}

func (s *EventStore) Replay(cursor int, limit int) ([]Event, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_RDONLY, 0600)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var events []Event
	decoder := json.NewDecoder(f)
	count := 0
	lastSeq := cursor
	for {
		var evt Event
		if err := decoder.Decode(&evt); err != nil {
			if err == io.EOF {
				break
			}
			return nil, 0, fmt.Errorf("malformed JSONL: %w", err)
		}
		if evt.Sequence <= cursor {
			continue
		}
		events = append(events, evt)
		lastSeq = evt.Sequence
		count++
		if limit > 0 && count >= limit {
			break
		}
	}

	return events, lastSeq, nil
}
