package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// MachineConfig is the Pix v2 config.toml schema
// (docs/design/pix-v2-architecture.md §5, §12.1): "config.toml contains only
// the default environment, selected local inference backend, and installed
// release manifest identity." The release manifest identity itself is a
// separate document (see the release package's InstallStatePath) — this type
// is the two remaining fields plus nothing else. There is no packs table,
// no plugins table, no host-mode table, and no XDG split: every function
// here takes the resolved Pix home root as a plain string parameter, the
// same convention the release package uses, so this file takes no
// intra-module dependency on pixhome (which would violate the L0 foundation
// ordering — see arch_test.go's l0Order: config and pixhome are both rank 0,
// and a same-rank import is refused).
type MachineConfig struct {
	// Environment is the machine default environment NAME, written only by
	// `pix env default NAME` (surface §3.4). Empty means no registered
	// default.
	Environment string `toml:"environment,omitempty"`

	// InferenceBackend is the selected local inference backend: "llmman",
	// "ollama", or "" for none selected yet (surface §7, architecture §12).
	// Setup is the only writer; Pix does not silently prefer or migrate
	// between them.
	InferenceBackend string `toml:"inference_backend,omitempty"`
}

// InferenceBackendLLMMan and InferenceBackendOllama are the two supported
// local inference backend values (surface §7).
const (
	InferenceBackendLLMMan = "llmman"
	InferenceBackendOllama = "ollama"
)

// ValidInferenceBackend reports whether s is empty (unselected) or one of
// the two supported backend names.
func ValidInferenceBackend(s string) bool {
	return s == "" || s == InferenceBackendLLMMan || s == InferenceBackendOllama
}

// MachineConfigFileName is config.toml's file name directly under PIX_HOME
// (architecture §5).
const MachineConfigFileName = "config.toml"

// MachineConfigPath resolves <home>/config.toml.
func MachineConfigPath(home string) string {
	return filepath.Join(home, MachineConfigFileName)
}

// LoadMachineConfig reads and decodes <home>/config.toml. A missing file
// returns a zero-value MachineConfig and a nil error: absence is "nothing
// configured yet", not a failure — the same posture the v1 Load() takes for
// its own config file. An unrecognized key is refused rather than silently
// dropped: this file has exactly two fields, so a typo or a stale writer
// producing a third is worth failing loudly over, and there is no packs/
// plugins clutter here that would make DisallowUnknownFields noisy in
// practice the way it would on the v1 schema.
func LoadMachineConfig(home string) (MachineConfig, error) {
	path := MachineConfigPath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MachineConfig{}, nil
		}
		return MachineConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c MachineConfig
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&c)
	if err != nil {
		return MachineConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return MachineConfig{}, fmt.Errorf("%s: unrecognized key %q — config.toml has no field named this in Pix v2", path, undecoded[0].String())
	}
	if !ValidInferenceBackend(c.InferenceBackend) {
		return MachineConfig{}, fmt.Errorf("%s: inference_backend %q is not one of %q, %q, or empty", path, c.InferenceBackend, InferenceBackendLLMMan, InferenceBackendOllama)
	}
	return c, nil
}

// SaveMachineConfig validates and durably writes c to <home>/config.toml: a
// same-directory temp file, fsync, then atomic rename — mirroring
// release.SaveInstalled and pixhome.Init's own durable-write patterns rather
// than importing either (see this file's package doc for why). Mode 0600:
// config.toml is machine state a Git commit should never see (pixhome's
// .gitignore already excludes it).
func SaveMachineConfig(home string, c MachineConfig) error {
	if !ValidInferenceBackend(c.InferenceBackend) {
		return fmt.Errorf("inference_backend %q is not one of %q, %q, or empty", c.InferenceBackend, InferenceBackendLLMMan, InferenceBackendOllama)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(&c); err != nil {
		return err
	}
	path := MachineConfigPath(home)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return machineAtomicWrite(dir, path, buf.Bytes(), 0o600)
}

// machineAtomicWrite writes data to a temp file in dir, fsyncs it, then
// renames it over path.
func machineAtomicWrite(dir, path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
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
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
