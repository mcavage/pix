// memoryport.go — per-PIX_HOME pix-memory port allocation. The chosen port is
// machine state at <PIX_HOME>/.state/memory/port, never user configuration.
package container

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pix/host/pixhome"
	"pix/host/sys"
)

// setupLockName is the advisory flock `pix setup` holds across port allocation
// and container reconciliation.
const setupLockName = ".setup.lock"

func SetupLockPath(home pixhome.Paths) string {
	return filepath.Join(home.Home, setupLockName)
}

func memoryPortPath(home pixhome.Paths) string {
	return filepath.Join(home.StateMemory, "port")
}

func readPersistedMemoryPort(home pixhome.Paths) (int, bool, error) {
	b, err := os.ReadFile(memoryPortPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || port < 1 || port > 65535 {
		return 0, false, fmt.Errorf("invalid pix-memory port in %s", memoryPortPath(home))
	}
	return port, true, nil
}

func persistMemoryPort(home pixhome.Paths, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid pix-memory port %d", port)
	}
	if err := os.MkdirAll(home.StateMemory, 0o700); err != nil {
		return err
	}
	return sys.AtomicWriteInDir(home.StateMemory, "port", []byte(strconv.Itoa(port)+"\n"), 0o600)
}

// EnsureMemoryPort returns the persisted per-home port, allocating it once.
func EnsureMemoryPort(home pixhome.Paths) (int, error) {
	var port int
	err := sys.Lock(SetupLockPath(home), func() error {
		p, err := AllocateMemoryPortLocked(home)
		port = p
		return err
	})
	return port, err
}

// AllocateMemoryPortLocked is the unlocked core. The caller holds SetupLockPath.
func AllocateMemoryPortLocked(home pixhome.Paths) (int, error) {
	if port, ok, err := readPersistedMemoryPort(home); err != nil {
		return 0, err
	} else if ok {
		return port, nil
	}
	port := DefaultMemoryPort
	if PortAvailable(port) != nil {
		free, err := freeLoopbackPort()
		if err != nil {
			return 0, err
		}
		port = free
	}
	if err := persistMemoryPort(home, port); err != nil {
		return 0, err
	}
	return port, nil
}

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

// ReadMemoryPort is read-only. An uninitialized home reports the preferred
// default without persisting it.
func ReadMemoryPort(home pixhome.Paths) (int, error) {
	if port, ok, err := readPersistedMemoryPort(home); err != nil {
		return 0, err
	} else if ok {
		return port, nil
	}
	return DefaultMemoryPort, nil
}

// ReallocateMemoryPortLocked chooses and persists a fresh free port. The caller
// holds SetupLockPath across this write and container reconciliation.
func ReallocateMemoryPortLocked(home pixhome.Paths) (int, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return 0, err
	}
	if err := persistMemoryPort(home, port); err != nil {
		return 0, err
	}
	return port, nil
}

func IsPortInUse(err error) bool {
	var pe *PortInUseError
	return errors.As(err, &pe)
}
