// memoryport.go — per-PIX_HOME pix-memory port allocation (QA F4: a single
// fixed 18080 forced two independent PIX_HOME instances on the same host
// into an unavoidable collision the moment both ran `pix setup`). The port
// is machine state now (config.Config.MemoryPort, the sole config.toml
// schema — round 5 deleted the short-lived pixhome-package struct duplicate
// that used to compete with it for the same file), allocated once by `pix setup`
// and read thereafter by everyone else — container, run's effective
// builtin, env preview, doctor, reset all read the SAME persisted value,
// never a re-derived one. Every persist below goes through config.WithLockAt,
// the ONE config.toml lock, so a concurrent `pix env default` writer can
// never lose an update racing this one (or vice versa).
package container

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"pix/host/config"
	"pix/host/pixhome"
	"pix/host/sys"
)

// setupLockName is the advisory flock `pix setup` holds for its whole
// memory-port-allocate-then-container-reconcile sequence.
const setupLockName = ".setup.lock"

// SetupLockPath is <home>/.setup.lock — the ONE lock a setup caller holds
// across BOTH allocating (or reading) the memory port AND the container
// create/reconcile that binds it, so a second concurrent `pix setup`
// against the SAME PIX_HOME can never observe a port this process has
// committed to but not yet asked Docker to bind (the residual race a
// port-allocation-only lock would leave open).
func SetupLockPath(home pixhome.Paths) string {
	return filepath.Join(home.Home, setupLockName)
}

// EnsureMemoryPort returns the persisted per-PIX_HOME loopback port
// pix-memory publishes on, allocating and durably recording one on first
// use. It is the whole contract for a standalone caller: idempotent (a
// second call returns the SAME value, never reallocates) and locked (two
// concurrent callers against the SAME home never race the check-then-save
// and never persist two different values — the second blocks on
// SetupLockPath, then simply reads back what the first already committed).
//
// A caller that also needs to reconcile the container in the same breath
// (`pix setup` itself) should hold SetupLockPath itself and call
// AllocateMemoryPortLocked instead, so the whole sequence — allocate AND
// create/start the container — serializes as one unit; see that function's
// doc for why a port-allocation-only lock is not enough on its own.
func EnsureMemoryPort(home pixhome.Paths) (int, error) {
	var port int
	err := sys.Lock(SetupLockPath(home), func() error {
		p, err := AllocateMemoryPortLocked(home)
		if err != nil {
			return err
		}
		port = p
		return nil
	})
	return port, err
}

// AllocateMemoryPortLocked is EnsureMemoryPort's unlocked core: it loads the
// machine config, returns the already-persisted port unchanged if one
// exists, or allocates and saves a fresh one. The CALLER must already hold
// SetupLockPath(home) (EnsureMemoryPort does this for a standalone caller;
// `pix setup` holds it itself across this AND container.Reconcile instead
// — QA F4's "hold the setup lock through container create/reconcile":
// without that, two concurrent `pix setup` runs against the SAME home could
// each allocate under EnsureMemoryPort's OWN lock, release it, and only THEN
// race each other's docker create of the identically-named container).
//
// Allocation prefers DefaultMemoryPort — the historical fixed value — ONLY
// when it is actually free right now: an existing single-install host keeps
// the port it always had, with no config.toml migration needed. When it is
// NOT free (a genuinely concurrent second PIX_HOME, or anything else
// already bound to it), a real free loopback port is drawn straight from
// the OS (bind 127.0.0.1:0, read back the assigned port, close — never
// held open past that, since Docker itself must bind the real address) so
// two independent PIX_HOME instances started at the same moment can never
// be steered onto the same port by a shared constant.
func AllocateMemoryPortLocked(home pixhome.Paths) (int, error) {
	var port int
	err := config.WithLockAt(home.Home, func(c *config.Config) error {
		if c.MemoryPort != 0 {
			port = c.MemoryPort
			return nil
		}
		p := DefaultMemoryPort
		if PortAvailable(p) != nil {
			free, ferr := freeLoopbackPort()
			if ferr != nil {
				return ferr
			}
			p = free
		}
		c.MemoryPort = p
		port = p
		return nil
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

// freeLoopbackPort asks the OS for a currently-unused loopback port: bind
// 127.0.0.1:0 (port 0 means "OS, you pick"), read back what it assigned,
// then close immediately — this process never holds the port open past the
// read, since the actual bind that matters is Docker's own.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate a free loopback port for pix-memory: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("allocate a free loopback port for pix-memory: unexpected listener address %v", ln.Addr())
	}
	return addr.Port, nil
}

// ReadMemoryPort returns the persisted per-PIX_HOME pix-memory port WITHOUT
// ever allocating one — the read-only half `pix doctor`, `pix run`'s
// effective builtin, and `pix env --effective` preview all use (safety
// invariant 12: a read-only command never mutates host state). A home that
// has never run `pix setup` (MemoryPort still zero) reports
// DefaultMemoryPort — the same "not ready yet, but here is what it WOULD
// be" display value these callers already show for an unresolvable
// pix-memory (see ReadMemoryAuthToken's own doc: a missing token degrades
// the same way).
func ReadMemoryPort(home pixhome.Paths) (int, error) {
	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		return 0, err
	}
	if c.MemoryPort != 0 {
		return c.MemoryPort, nil
	}
	return DefaultMemoryPort, nil
}

// ReallocateMemoryPortLocked draws a FRESH free loopback port, persists it
// as this PIX_HOME's memory port, and returns it. The CALLER must already
// hold SetupLockPath(home).
//
// This is the recovery half of the cross-PIX_HOME collision QA F4 opened
// (two homes both allocated 18080 because it was free at ALLOCATION time,
// then the second one's `docker create` lost the bind race). Allocation
// alone cannot close that window — the only authority on "is this port
// actually bindable" is the bind itself — so `pix setup` retries here when
// reconcile reports PortInUseError, atomically committing the new port to
// config.toml BEFORE the retry so the container it creates and the Gateway
// URL it registers can never disagree about which port pix-memory answers
// on.
func ReallocateMemoryPortLocked(home pixhome.Paths) (int, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return 0, err
	}
	err = config.WithLockAt(home.Home, func(c *config.Config) error {
		c.MemoryPort = port
		return nil
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

// IsPortInUse reports whether err is (or wraps) the bind conflict
// PortAvailable raises — the ONE reconcile failure a fresh port can fix. Any
// other error (no Docker, an image that will not pull, a refused replace)
// is returned to the user unchanged; retrying those on a new port would
// just fail four more times with a less honest message.
func IsPortInUse(err error) bool {
	var pe *PortInUseError
	return errors.As(err, &pe)
}
