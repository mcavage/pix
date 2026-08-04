// tree.go — the root supervisor, the per-unit child supervisors, and the typed
// status/event surface the rest of pix-host reads instead of grepping logs.

package supervise

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/thejerf/suture/v4"
)

// UnitState is the typed lifecycle state of one supervised unit.
type UnitState string

const (
	UnitStarting UnitState = "starting" // spawning/reattaching, not yet healthy
	UnitRunning  UnitState = "running"  // up and passing its health probe
	UnitDegraded UnitState = "degraded" // up but failing probes (not yet evicted)
	UnitBackoff  UnitState = "backoff"  // Suture is holding off a restart
	UnitStopped  UnitState = "stopped"  // stopped on purpose
	UnitFailed   UnitState = "failed"   // permanently out (ErrDoNotRestart)
)

// UnitStatus is a snapshot of one unit. Copy semantics: callers get a value.
type UnitStatus struct {
	Name        string
	Kind        string
	State       UnitState
	PID         int
	HealthOK    bool
	Reattached  bool
	Restarts    int
	Generations int
	LastError   string
	Since       time.Time
}

// EventType is the typed vocabulary of supervision events.
type EventType string

const (
	EventStarted          EventType = "started"
	EventReattached       EventType = "reattached"
	EventReattachRejected EventType = "reattach_rejected"
	EventHealthFailed     EventType = "health_failed"
	EventExited           EventType = "exited"
	EventBackoff          EventType = "backoff"
	EventResume           EventType = "resume"
	EventDrainTimeout     EventType = "drain_timeout"
	EventStopTimeout      EventType = "stop_timeout"
	EventStopped          EventType = "stopped"
	EventDoNotRestart     EventType = "do_not_restart"
	EventPanic            EventType = "panic"
)

// Event is one typed supervision event.
type Event struct {
	Time    time.Time
	Unit    string
	Type    EventType
	Message string
	Err     string
}

// eventRing caps retained events; a supervisor must never grow without bound.
const eventRing = 256

// Config builds a Tree. Everything is explicit so a test can point the tree at
// temp dirs and shrunk budgets without touching global state.
type Config struct {
	SelfPath  string // this binary, re-exec'd for self-exec units
	StageDir  string // where external unit binaries are staged + verified
	StateDir  string // where reattach state is persisted
	Budgets   Budgets
	Plugins   map[string]goplugin.Plugin
	Handshake goplugin.HandshakeConfig
	EventSink func(Event) // optional: every typed event, as it happens
	Logf      func(string, ...any)
}

// Tree is the root supervisor plus one child supervisor per unit.
type Tree struct {
	root      *suture.Supervisor
	budgets   Budgets
	selfPath  string
	stageDir  string
	stateDir  string
	plugins   map[string]goplugin.Plugin
	handshake goplugin.HandshakeConfig
	sink      func(Event)
	logger    func(string, ...any)

	mu      sync.Mutex
	units   map[string]*UnitStatus
	order   []string
	events  []Event
	started bool
	done    chan struct{}
	cancel  context.CancelFunc
}

// NewTree builds the root supervisor. Nothing runs until Start.
func NewTree(cfg Config) *Tree {
	b := cfg.Budgets
	if b.HealthInterval == 0 {
		b = DefaultBudgets()
	}
	t := &Tree{
		budgets: b, selfPath: cfg.SelfPath, stageDir: cfg.StageDir, stateDir: cfg.StateDir,
		plugins: cfg.Plugins, handshake: cfg.Handshake, sink: cfg.EventSink, logger: cfg.Logf,
		units: map[string]*UnitStatus{},
	}
	// DontPropagateTermination: a unit that asks for its tree to terminate takes
	// out its OWN subtree only — the root (and therefore every other unit) is
	// never killed by one bad unit.
	t.root = suture.New("pix-host", suture.Spec{
		EventHook:                t.sutureHook(""),
		FailureBackoff:           b.FailureBackoff,
		FailureThreshold:         b.FailureThreshold,
		FailureDecay:             b.FailureDecay,
		Timeout:                  b.Stop,
		DontPropagateTermination: true,
	})
	return t
}

func (t *Tree) logf(format string, a ...any) {
	if t.logger != nil {
		t.logger(format, a...)
		return
	}
	log.Printf(format, a...)
}

// Start runs the root supervisor in the background. Cancelling ctx stops every
// unit; Stop waits for that to finish.
func (t *Tree) Start(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.started = true
	t.done = make(chan struct{})
	errCh := t.root.ServeBackground(cctx)
	go func() {
		if err := <-errCh; err != nil && err != context.Canceled {
			t.logf("supervise: root supervisor exited: %v", err)
		}
		close(t.done)
	}()
}

// Add supervises a unit and BLOCKS until its first generation is healthy, or
// its first start attempt fails. A misconfigured unit therefore fails `serve`
// at startup (loudly, with the real error) instead of flapping in the dark; a
// unit that merely dies later is Suture's problem, not the caller's.
func (t *Tree) Add(spec UnitSpec, health HealthFunc) (*Holder, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return nil, fmt.Errorf("supervise: tree not started")
	}
	if _, dup := t.units[spec.Name]; dup {
		t.mu.Unlock()
		return nil, fmt.Errorf("supervise: unit %q already supervised", spec.Name)
	}
	t.units[spec.Name] = &UnitStatus{Name: spec.Name, Kind: spec.Kind, State: UnitStarting, Since: time.Now()}
	t.order = append(t.order, spec.Name)
	t.mu.Unlock()

	svc := &GoPluginService{spec: spec, health: health, holder: &Holder{}, tree: t, ready: make(chan error, 1)}
	// One child supervisor per unit: its restart accounting, backoff and
	// permanent death are its own.
	child := suture.New("unit."+spec.Name, suture.Spec{
		EventHook:                t.sutureHook(spec.Name),
		FailureBackoff:           t.budgets.FailureBackoff,
		FailureThreshold:         t.budgets.FailureThreshold,
		FailureDecay:             t.budgets.FailureDecay,
		Timeout:                  t.budgets.Stop,
		DontPropagateTermination: true,
	})
	child.Add(svc)
	t.root.Add(child)

	select {
	case err := <-svc.ready:
		if err != nil {
			return nil, err
		}
		return svc.holder, nil
	case <-time.After(t.budgets.Handshake + t.budgets.HealthTimeout):
		return nil, fmt.Errorf("unit %s: did not become healthy within %v", spec.Name, t.budgets.Handshake)
	}
}

// Stop cancels every unit and waits for the root supervisor to finish. Safe to
// call more than once, and safe with nothing started.
func (t *Tree) Stop() {
	t.mu.Lock()
	cancel, done, started := t.cancel, t.done, t.started
	t.mu.Unlock()
	if !started {
		return
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(t.budgets.Stop + t.budgets.Drain):
		t.logf("supervise: root supervisor did not stop within %v", t.budgets.Stop+t.budgets.Drain)
	}
	// Belt and braces for any client that outlived its Serve goroutine.
	goplugin.CleanupClients()
}

// SetSelfPath records this binary's path, used to launch self-exec units. It
// is a no-op for an empty path so a caller that could not resolve os.Executable
// cannot blank an already-known one.
func (t *Tree) SetSelfPath(path string) {
	if path == "" {
		return
	}
	t.mu.Lock()
	t.selfPath = path
	t.mu.Unlock()
}

// SelfPath is the recorded path of this binary.
func (t *Tree) SelfPath() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selfPath
}

// Status returns a snapshot of every unit, in the order they were added.
func (t *Tree) Status() []UnitStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]UnitStatus, 0, len(t.order))
	for _, n := range t.order {
		if st := t.units[n]; st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// Unit returns one unit's status snapshot.
func (t *Tree) Unit(name string) (UnitStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.units[name]
	if !ok {
		return UnitStatus{}, false
	}
	return *st, true
}

// Events returns the retained typed events, oldest first.
func (t *Tree) Events() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Event, len(t.events))
	copy(out, t.events)
	return out
}

// transition mutates one unit's status under the lock.
func (t *Tree) transition(name string, fn func(*UnitStatus)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.units[name]
	if !ok {
		return
	}
	before := st.State
	fn(st)
	if st.State != before {
		st.Since = time.Now()
	}
}

// fail records a transient failure (Suture will restart the unit).
func (t *Tree) fail(name string, err error) {
	t.transition(name, func(st *UnitStatus) {
		st.State, st.PID, st.HealthOK, st.LastError = UnitBackoff, 0, false, err.Error()
	})
}

// emit records a typed event (bounded ring) and fans it out to the sink.
func (t *Tree) emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	t.mu.Lock()
	t.events = append(t.events, e)
	if len(t.events) > eventRing {
		t.events = t.events[len(t.events)-eventRing:]
	}
	sink := t.sink
	t.mu.Unlock()
	if sink != nil {
		sink(e)
	}
}

// sutureHook translates Suture's own events into the typed vocabulary, so
// backoff/resume/stop-timeout/panic are visible in `Events()` alongside the
// ones this package raises — one stream, one shape.
func (t *Tree) sutureHook(unit string) suture.EventHook {
	return func(ev suture.Event) {
		e := Event{Unit: unit, Message: ev.String()}
		switch v := ev.(type) {
		case suture.EventBackoff:
			e.Type = EventBackoff
		case suture.EventResume:
			e.Type = EventResume
		case suture.EventStopTimeout:
			e.Type = EventStopTimeout
		case suture.EventServicePanic:
			e.Type, e.Err = EventPanic, v.PanicMsg
		case suture.EventServiceTerminate:
			e.Type = EventExited
			e.Err = fmt.Sprint(v.Err)
			if !v.Restarting {
				e.Type = EventDoNotRestart
			}
		default:
			e.Type = EventType(fmt.Sprintf("suture_%d", ev.Type()))
		}
		t.emit(e)
	}
}

// SortedUnitNames is a small helper for callers rendering status tables.
func SortedUnitNames(sts []UnitStatus) []string {
	out := make([]string, 0, len(sts))
	for _, s := range sts {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
