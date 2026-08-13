package uat

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type EventType string

const (
	EventStatus    EventType = "status"
	EventStepStart EventType = "step_start"
	EventStepDone  EventType = "step_done"
	EventRunDone   EventType = "run_done"
)

type Event struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      EventType `json:"type"`
	Message   string    `json:"message,omitempty"`
	State     string    `json:"state,omitempty"` // pass/fail/incomplete/cancelled/timed-out
}

type EventLog struct {
	mu     sync.Mutex
	path   string
	nextID int64
}

func NewEventLog(path string) *EventLog {
	return &EventLog{path: path, nextID: 1}
}

func (e *EventLog) Append(evt Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	evt.ID = e.nextID
	e.nextID++
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(b)
	return err
}

func (e *EventLog) ReadSince(cursor int64) ([]Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// basic implementation
	b, err := os.ReadFile(e.path)
	if err != nil {
		return nil, err
	}
	var events []Event
	for len(b) > 0 {
		var evt Event
		// find newline
		idx := -1
		for i := 0; i < len(b); i++ {
			if b[i] == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			break
		}
		line := b[:idx]
		b = b[idx+1:]
		if err := json.Unmarshal(line, &evt); err == nil {
			if evt.ID > cursor {
				events = append(events, evt)
			}
		} else {
			return nil, fmt.Errorf("malformed event log JSON: %w", err)
		}
	}
	return events, nil
}
