package pixhome

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// machine.go is the v2 config.toml reader/writer (docs/design/
// pix-v2-surface.md §4.1, pix-v2-architecture.md §5): "config.toml contains
// only machine-wide Pix choices that native sbx environments cannot own."
// This first field is the default environment name, written by `pix env
// default NAME`. Other machine-wide fields (selected local inference
// backend, pinned release identity) get their own named writer when a
// caller needs one; there is no generic config mutation command.

// Machine is the whole of <PIX_HOME>/config.toml today. Every field has
// exactly one named writer (surface §4.1); there is no generic set/unset.
type Machine struct {
	// DefaultEnvironment is the machine default environment name, written
	// by `pix env default NAME` and read by every launch that does not
	// pass an explicit --env.
	DefaultEnvironment string `toml:"default_environment"`
}

// LoadMachine reads <home>/config.toml. A missing file is the legitimate
// "nothing configured yet" state: it returns a zero Machine and a nil
// error, never an error a fresh host would trip on.
func LoadMachine(home Paths) (Machine, error) {
	var m Machine
	data, err := os.ReadFile(home.ConfigTOML)
	if err != nil {
		if os.IsNotExist(err) {
			return Machine{}, nil
		}
		return Machine{}, fmt.Errorf("read %s: %w", home.ConfigTOML, err)
	}
	if _, err := toml.Decode(string(data), &m); err != nil {
		return Machine{}, fmt.Errorf("parse %s: %w", home.ConfigTOML, err)
	}
	return m, nil
}

// SaveMachine writes m to <home>/config.toml atomically (same-directory
// temp file + rename), mode 0600 — this file carries machine state, never
// tracked by the initialized Git repository (surface §4).
func SaveMachine(home Paths, m Machine) error {
	if err := os.MkdirAll(home.Home, dirMode); err != nil {
		return fmt.Errorf("create %s: %w", home.Home, err)
	}
	tmp, err := os.CreateTemp(home.Home, ".config.toml.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(m); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, home.ConfigTOML); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// SetDefaultEnvironment loads, mutates, and saves the one field `pix env
// default NAME` owns. It is not lock-protected against a concurrent writer
// (config.toml has no other v2 writer yet); a future second writer adds
// locking here rather than at each call site.
func SetDefaultEnvironment(home Paths, name string) error {
	m, err := LoadMachine(home)
	if err != nil {
		return err
	}
	m.DefaultEnvironment = name
	return SaveMachine(home, m)
}
