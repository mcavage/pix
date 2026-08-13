package uat

import (
	"bufio"
	"encoding/json"
	"os"
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

func NewEventStore(path string) (*EventStore, error) {
	// Ensure file exists with 0600 permissions
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &EventStore{path: path}, nil
}

func (s *EventStore) Append(eventType string, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_RDWR, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Lock the file for cross-process safety
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Read last sequence
	scanner := bufio.NewScanner(f)
	lastSeq := 0
	for scanner.Scan() {
		var evt Event
		if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
			if evt.Sequence > lastSeq {
				lastSeq = evt.Sequence
			}
		}
	}
	newSeq := lastSeq + 1

	// Append new event
	evt := Event{Sequence: newSeq, Type: eventType, Data: data}
	b, err := json.Marshal(evt)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, 2); err != nil {
		return 0, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return 0, err
	}

	return newSeq, nil
}
