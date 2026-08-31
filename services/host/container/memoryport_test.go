package container

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"pix/host/pixhome"
)

// TestEnsureMemoryPort_RerunStable: a second call against the SAME home
// returns the exact same port a first call already allocated and persisted
// — no reallocation, no drift across `pix setup` reruns.
func TestEnsureMemoryPort_RerunStable(t *testing.T) {
	home := pixhome.New(t.TempDir())
	p1, err := EnsureMemoryPort(home)
	if err != nil {
		t.Fatalf("EnsureMemoryPort: %v", err)
	}
	if p1 == 0 {
		t.Fatal("expected a non-zero allocated port")
	}
	p2, err := EnsureMemoryPort(home)
	if err != nil {
		t.Fatalf("EnsureMemoryPort (rerun): %v", err)
	}
	if p2 != p1 {
		t.Fatalf("port changed across a rerun: %d != %d", p1, p2)
	}
	m, err := pixhome.LoadMachine(home)
	if err != nil {
		t.Fatalf("LoadMachine: %v", err)
	}
	if m.MemoryPort != p1 {
		t.Errorf("persisted MemoryPort = %d, want %d", m.MemoryPort, p1)
	}
}

// TestEnsureMemoryPort_PrefersDefaultWhenFree: a fresh home with the
// canonical default free adopts it verbatim — an existing single-install
// host must never be migrated onto a different port just because F4 shipped.
func TestEnsureMemoryPort_PrefersDefaultWhenFree(t *testing.T) {
	if PortAvailable(DefaultMemoryPort) != nil {
		t.Skipf("port %d is not free on this test host; cannot prove the preference", DefaultMemoryPort)
	}
	home := pixhome.New(t.TempDir())
	got, err := EnsureMemoryPort(home)
	if err != nil {
		t.Fatalf("EnsureMemoryPort: %v", err)
	}
	if got != DefaultMemoryPort {
		t.Errorf("port = %d, want the canonical default %d (it was free)", got, DefaultMemoryPort)
	}
}

// TestEnsureMemoryPort_TwoHomesDistinctPortsWhileFirstListenerHeld is F4's
// core collision proof: hold a REAL listener on the canonical default port
// (standing in for one PIX_HOME's already-running pix-memory container),
// then allocate for a SECOND, independent PIX_HOME. It must land on a
// DIFFERENT port — never the one already held — and that second port must
// itself actually be free right now.
func TestEnsureMemoryPort_TwoHomesDistinctPortsWhileFirstListenerHeld(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a listener to simulate home #1's running container: %v", err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	// Home #1 "already has" the held port recorded (as if `pix setup` ran
	// earlier and its container is still bound to it).
	home1 := pixhome.New(t.TempDir())
	if err := pixhome.SaveMachine(home1, pixhome.Machine{MemoryPort: held}); err != nil {
		t.Fatalf("seed home #1: %v", err)
	}

	// Home #2 is a genuinely independent, fresh PIX_HOME allocating for the
	// first time while home #1's port is held.
	home2 := pixhome.New(t.TempDir())
	got2, err := EnsureMemoryPort(home2)
	if err != nil {
		t.Fatalf("EnsureMemoryPort (home #2): %v", err)
	}
	if got2 == held {
		t.Fatalf("home #2 allocated the held port %d; two independent PIX_HOME instances must never collide", held)
	}
	if err := PortAvailable(got2); err != nil {
		t.Errorf("home #2's allocated port %d is not actually free: %v", got2, err)
	}
}

// TestEnsureMemoryPort_ConcurrentSameHomeOneValue: N goroutines calling
// EnsureMemoryPort against the SAME home concurrently must all observe the
// exact same allocated port — the setup lock serializes the check-then-save,
// so there is never a window where two different values could each look
// unset and each persist their own guess.
func TestEnsureMemoryPort_ConcurrentSameHomeOneValue(t *testing.T) {
	home := pixhome.New(t.TempDir())
	const n = 12
	ports := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ports[i], errs[i] = EnsureMemoryPort(home)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureMemoryPort[%d]: %v", i, err)
		}
	}
	want := ports[0]
	if want == 0 {
		t.Fatal("allocated port must not be zero")
	}
	for i, p := range ports {
		if p != want {
			t.Errorf("ports[%d] = %d, want %d (every concurrent caller against one home must agree)", i, p, want)
		}
	}
}

// TestReadMemoryPort_PreSetupDefaultsWithoutAllocating: the read-only half
// never mutates state — a home that has never run `pix setup` reports the
// canonical default for display, but config.toml stays absent/untouched.
func TestReadMemoryPort_PreSetupDefaultsWithoutAllocating(t *testing.T) {
	home := pixhome.New(t.TempDir())
	got, err := ReadMemoryPort(home)
	if err != nil {
		t.Fatalf("ReadMemoryPort: %v", err)
	}
	if got != DefaultMemoryPort {
		t.Errorf("ReadMemoryPort (pre-setup) = %d, want the display default %d", got, DefaultMemoryPort)
	}
	m, err := pixhome.LoadMachine(home)
	if err != nil {
		t.Fatalf("LoadMachine: %v", err)
	}
	if m.MemoryPort != 0 {
		t.Errorf("ReadMemoryPort must never allocate: MemoryPort = %d, want 0", m.MemoryPort)
	}
}

// TestReadMemoryPort_ReflectsAllocated proves the read-only and allocating
// halves never disagree once a port exists.
func TestReadMemoryPort_ReflectsAllocated(t *testing.T) {
	home := pixhome.New(t.TempDir())
	allocated, err := EnsureMemoryPort(home)
	if err != nil {
		t.Fatalf("EnsureMemoryPort: %v", err)
	}
	got, err := ReadMemoryPort(home)
	if err != nil {
		t.Fatalf("ReadMemoryPort: %v", err)
	}
	if got != allocated {
		t.Errorf("ReadMemoryPort = %d, want the allocated %d", got, allocated)
	}
}

// TestReconcile_PortCollisionGivesExactSetupRemedy: when the port this spec
// names is already held by something else at the moment Reconcile tries to
// create the container, the refusal names the exact remedy — never an
// opaque `docker create` failure surfacing three layers down. This is the
// backstop for the narrow residual race F4's doc callout describes: even if
// two independent allocations ever did land on the same value, Reconcile's
// own pre-create probe still catches it and says exactly what to do.
func TestReconcile_PortCollisionGivesExactSetupRemedy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a colliding listener: %v", err)
	}
	defer ln.Close()
	collidingPort := ln.Addr().(*net.TCPAddr).Port

	spec := Spec{ContainerName: "pix-memory-test", Image: "img", HostPort: collidingPort, DataDir: t.TempDir()}
	r := &fakeRunner{}
	r.script("inspect", "Error: No such object: pix-memory-test", errors.New("exit status 1"))
	_, err = Reconcile(r, spec, nil, ReconcileOptions{})
	if err == nil {
		t.Fatal("expected Reconcile to refuse a port already in use")
	}
	portErr, ok := err.(*PortInUseError)
	if !ok {
		t.Fatalf("Reconcile error = %v (%T), want a *PortInUseError naming the exact remedy", err, err)
	}
	if portErr.Port != collidingPort {
		t.Errorf("PortInUseError.Port = %d, want %d", portErr.Port, collidingPort)
	}
	if got := portErr.Error(); !strings.Contains(got, "pix setup") {
		t.Errorf("PortInUseError message = %q, want it to name the exact remedy (rerun `pix setup`)", got)
	}
}
