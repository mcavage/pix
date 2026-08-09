// kits.go — kit lifecycle for a launch: which release ref the auto-pin
// resolves (ResolveKitRef), the personal-context mixin synthesized from the
// user's durable AGENTS.md, schema-compat validation of local kits before
// `sbx run` opens any interactive setup, and cleanup of the transient kit dirs
// the launcher itself synthesized.
package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/launcher"
)

// kitref.go — which release does `pix run` actually run? Precedence is
// --kit-ref, then version_pin, then the launcher's stamped version. There is
// deliberately no run-time "latest release" lookup: Homebrew is the
// user-initiated update channel, so the kit and image move only when the
// launcher moves, and kit metadata never changes without an explicit upgrade.

type KitRefSource int

const (
	KitRefStamped    KitRefSource = iota // this binary's own version (default / every fallback)
	KitRefFlag                           // --kit-ref
	KitRefConfigPin                      // version_pin in config.toml
	KitRefUnreleased                     // dev/local build: main or a local checkout
)

// ResolveKitRef applies the precedence chain and reports which rule won.
func ResolveKitRef(version, flagRef, pinRef string) (ref string, src KitRefSource) {
	switch {
	case flagRef != "":
		return flagRef, KitRefFlag
	case pinRef != "":
		return NormalizeKitRef(pinRef), KitRefConfigPin
	case !launcher.IsReleased(version):
		return "", KitRefUnreleased
	default:
		return "", KitRefStamped
	}
}

func NormalizeKitRef(pin string) string {
	pin = strings.TrimSpace(pin)
	if launcher.IsReleased(pin) {
		return "v" + pin
	}
	return pin
}

func KitRefNotice(version, ref string, src KitRefSource) string {
	switch src {
	case KitRefConfigPin:
		return fmt.Sprintf("pix: kit %s (pinned by version_pin in config.toml)", ref)
	default:
		return ""
	}
}

func SynthesizePersonalContextKit() (string, error) {
	source := filepath.Join(config.ContextDir(), "AGENTS.md")
	b, err := os.ReadFile(source)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", nil
	}
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(state, "context-kits")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "personal-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	var spec strings.Builder
	spec.WriteString("schemaVersion: \"2\"\nkind: mixin\nname: pix-personal-context\nagentInstructions:\n  content: |\n")
	for _, line := range strings.Split(string(b), "\n") {
		fmt.Fprintf(&spec, "    %s\n", line)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec.String()), 0o600); err != nil {
		return "", err
	}
	complete = true
	return dir, nil
}

// ValidateCreateKits asks the installed sbx parser to validate local kits
// before `sbx run` performs any interactive/global setup. This catches
// nightly schema skew without opening a policy wizard and ending in a raw
// YAML decoder dump. Remote kits remain sbx's responsibility because
// validating them here would add an eager network fetch.
func ValidateCreateKits(args []string, validate func(string) (string, error)) error {
	if validate == nil {
		return nil
	}
	for _, ref := range LocalKitArgs(args) {
		out, err := validate(ref)
		if err == nil {
			continue
		}
		detail := firstOutputLine(out)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("this Pix kit does not match the installed Docker Sandboxes schema (%s)\n  detail: %s\n  fix: update Pix and Docker Sandboxes, then retry", ref, detail)
	}
	return nil
}

func LocalKitArgs(args []string) []string {
	var refs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--kit" {
			continue
		}
		ref := args[i+1]
		i++
		if filepath.IsAbs(ref) || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
			refs = append(refs, ref)
		}
	}
	return refs
}

func firstOutputLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func ValidateSbxKit(ref string) (string, error) {
	b, err := exec.Command("sbx", "kit", "validate", ref).CombinedOutput()
	return string(b), err
}

func ValidateSetupKit(version string, repoRoot func() (string, error), validate func(string) (string, error)) error {
	if launcher.IsReleased(version) || repoRoot == nil {
		return nil
	}
	root, err := repoRoot()
	if err != nil {
		return nil // run will use the remote main kit
	}
	return ValidateCreateKits([]string{"--kit", filepath.Join(root, "pi-kit")}, validate)
}

func CleanupGeneratedKitDirs(paths []string) error {
	seen := map[string]bool{}
	var errs []error
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove generated kit %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// EnsurePersonalContextDir creates the personal context tree (<context>/skills)
// if it does not exist, so the mount is always there.
//
// This exists because of a genuine chicken-and-egg: the dir used to be mounted
// only when it already had entries, which meant the FIRST skill could never be
// written from inside a sandbox. There was nowhere to write it, and the fix ("go
// create it on the host, then relaunch") contradicts the whole point of doing
// work in the sandbox.
//
// Best-effort by design: a failure here must never block a launch. The worst
// case is the previous behavior, an unmounted personal dir, which is a missing
// convenience and not a broken session.
func EnsurePersonalContextDir() {
	_ = os.MkdirAll(PersonalSkillsDir(), 0o700)
}
