package uat

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	hostuat "pix/host/uat"
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
	State     string    `json:"state,omitempty"`
}

type EventLog struct {
	store *hostuat.EventStore
	// err is why the store could not be opened. KEPT rather than dropped: the
	// constructor has no error return its callers could act on, and handing back
	// a nil store as if it were a working log is what turned an unopenable event
	// file into a SIGSEGV — twice over, because executeAsync's deferred handler
	// appends as well, so the panic it exists to record was replaced by an
	// identical one it could not recover from.
	err error
}

func NewEventLog(path string) *EventLog {
	store, err := hostuat.NewEventStore(path)
	return &EventLog{store: store, err: err}
}

// unavailable explains a log that was never opened. It tolerates a nil err so
// the reason can never itself render as "%!w(<nil>)".
func (e *EventLog) unavailable() error {
	if e.err != nil {
		return fmt.Errorf("event log unavailable: %w", e.err)
	}
	return errors.New("event log unavailable")
}

func (e *EventLog) Append(evt Event) error {
	if e.store == nil {
		return e.unavailable()
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = e.store.Append(string(evt.Type), b)
	return err
}

func (e *EventLog) ReadSince(cursor int64) ([]Event, error) {
	if e.store == nil {
		return nil, e.unavailable()
	}
	evts, _, err := e.store.Replay(int(cursor), 0)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, raw := range evts {
		var myEvt Event
		json.Unmarshal(raw.Data, &myEvt)
		myEvt.ID = int64(raw.Sequence)
		out = append(out, myEvt)
	}
	return out, nil
}
