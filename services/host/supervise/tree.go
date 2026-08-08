// tree.go — the root supervisor, the per-unit child supervisors, and the typed status/event surface the rest of pix-host reads instead of grepping logs.

package supervise

import (
	"context"
	"fmt"
	"log"
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
	UnitFailed   UnitState = "failed"   // permanently out of the tree
)

// UnitStatus is a snapshot of one unit. Callers get a value, not a pointer.
type UnitStatus struct {
	Name        string
	Kind        string
	Identity    string // admission fingerprint (sha256 hex of the spec; grant VALUES enter only as digests)
	State       UnitState
	PID         int
	HealthOK    bool
	Reattached  bool
	Restarts    int
	Generations int
	LastError   string
	LastProbeUS int64 // wall time of the most recent health probe, in MICROSECONDS (a healthy local probe rounds to 0ms); the unit's latency SLI
	Since       time.Time
	token       suture.ServiceToken // internal: which child Remove takes back out
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
	// EventOrphanKilled fires when tryReattach has already proven a persisted
	// pid is OUR OWN previously-launched unit (identity, protocol version, a
	// uid-owned process AND a uid-owned unix socket at the recorded address all
	// matched) but the reattach RPC itself still failed, and the pid was
	// therefore terminated directly rather than left running and holding
	// whatever the unit exclusively owns (memory's store flock is the
	// motivating case) forever.
	EventOrphanKilled EventType = "orphan_killed"
	// EventOrphanKillFailed fires when tryReattach revalidated and attempted
	// to kill a verified orphan (see EventOrphanKilled) but could not confirm
	// the process is actually gone: either the kill signal itself failed to
	// deliver, or the process was still alive after the kill wait budget.
	// Distinct from EventOrphanKilled on purpose — a caller must never read
	// "we tried to kill it" as "it is dead".
	EventOrphanKillFailed EventType = "orphan_kill_failed"
)

// Event is one typed supervision event.
type Event struct {
	Time    time.Time
	Unit    string
	Type    EventType
	Message string
	Err     string
}

const eventRing = 256 // caps retained events; a supervisor must never grow without bound

// Config builds a Tree; everything is explicit so a test can point it at temp dirs and shrunk budgets without touching global state.
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
	t.root = suture.New("pix-host", t.spec(""))
	return t
}

// spec is the Suture spec the root and every child supervisor run under. DontPropagateTermination: a unit that terminates takes out its OWN subtree only — one bad unit never kills the root or a sibling.
func (t *Tree) spec(unit string) suture.Spec {
	return suture.Spec{
		EventHook:                t.sutureHook(unit),
		FailureBackoff:           t.budgets.FailureBackoff,
		FailureThreshold:         t.budgets.FailureThreshold,
		FailureDecay:             t.budgets.FailureDecay,
		Timeout:                  t.budgets.Stop,
		DontPropagateTermination: true,
	}
}
func (t *Tree) logf(format string, a ...any) {
	if t.logger != nil {
		t.logger(format, a...)
		return
	}
	log.Printf(format, a...)
}

// Start runs the root supervisor in the background; Stop waits for it.
func (t *Tree) Start(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	t.cancel, t.started, t.done = cancel, true, make(chan struct{})
	errCh := t.root.ServeBackground(cctx)
	go func() {
		if err := <-errCh; err != nil && err != context.Canceled {
			t.logf("supervise: root supervisor exited: %v", err)
		}
		close(t.done)
	}()
}

// Add supervises a unit and BLOCKS until its first generation is healthy, or that attempt fails loudly at `serve` startup; a unit that dies later is Suture's problem.
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
	t.units[spec.Name] = &UnitStatus{Name: spec.Name, Kind: spec.Kind, Identity: spec.identity(), State: UnitStarting, Since: time.Now()}
	t.mu.Unlock()
	svc := &GoPluginService{spec: spec, health: health, holder: &Holder{}, tree: t, ready: make(chan error, 1)}
	// One child supervisor per unit: its restart accounting, backoff and permanent death are its own.
	child := suture.New("unit."+spec.Name, t.spec(spec.Name))
	child.Add(svc)
	token := t.root.Add(child)
	t.mu.Lock()
	t.units[spec.Name].token = token
	t.mu.Unlock()

	wait := t.budgets.Handshake + t.budgets.HealthTimeout
	select {
	case err := <-svc.ready:
		if err == nil {
			return svc.holder, nil
		}
		return nil, t.abandon(spec.Name, token, err)
	case <-time.After(wait):
		return nil, t.abandon(spec.Name, token,
			fmt.Errorf("unit %s: did not become healthy within %v", spec.Name, wait))
	}
}

// abandon takes a unit that never came up back OUT of the tree — without it the child supervisor keeps restarting a unit whose caller already gave up forever.
func (t *Tree) abandon(name string, token suture.ServiceToken, cause error) error {
	if err := t.root.RemoveAndWait(token, t.budgets.Stop); err != nil {
		t.logf("supervise: unit %s did not leave the tree cleanly: %v", name, err)
	}
	t.transition(name, func(st *UnitStatus) {
		st.State, st.PID, st.HealthOK, st.LastError = UnitFailed, 0, false, cause.Error()
	})
	t.emit(Event{Unit: name, Type: EventStopped, Message: "removed from the tree after a failed start", Err: cause.Error()})
	return cause
}

// Stop cancels every unit and waits for the root supervisor; safe to call more than once, or with nothing started.
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

// Remove stops and forgets one supervised unit — reconciliation's vocabulary for "no longer wanted" (dropped from a desired set, or superseded ahead of a same-name Add with a changed spec). A name never supervised is a no-op.
func (t *Tree) Remove(name string) error {
	t.mu.Lock()
	st, ok := t.units[name]
	t.mu.Unlock()
	if !ok {
		return nil
	}
	err := t.root.RemoveAndWait(st.token, t.budgets.Stop)
	t.mu.Lock()
	delete(t.units, name)
	t.mu.Unlock()
	return err
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
	return append([]Event(nil), t.events...)
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

// sutureHook translates Suture's own events into the typed vocabulary (backoff/resume/stop-timeout/panic), so Events() is one stream.
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
			e.Type, e.Err = EventExited, fmt.Sprint(v.Err)
			if !v.Restarting {
				e.Type = EventDoNotRestart
			}
		default:
			e.Type = EventType(fmt.Sprintf("suture_%d", ev.Type()))
		}
		t.emit(e)
	}
}
