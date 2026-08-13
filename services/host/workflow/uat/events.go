package uat

import (
	"encoding/json"
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
}

func NewEventLog(path string) *EventLog {
	store, _ := hostuat.NewEventStore(path)
	return &EventLog{store: store}
}

func (e *EventLog) Append(evt Event) error {
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
