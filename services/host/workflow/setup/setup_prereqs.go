package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/workflow/doctor"
	"runtime"
	"strings"
)

var SetupHostOS = runtime.GOOS

const (
	setupKitAllowedSource = "github.com/mcavage/"
	setupKitSourcesKey    = "kit.allowedSources"
)

// EnsureSetupPrereqs installs only Pix's two core host tools on macOS. Optional
// capabilities such as Ollama, gh, and gog are intentionally absent here.
// Interactive setup asks once for the package category; unattended setup never
// installs and returns exact commands instead.
func EnsureSetupPrereqs(env hostenv.Env, in io.Reader, out io.Writer, interactive bool) error {
	return EnsureSetupPrereqsFor(env, in, out, interactive, true)
}

// EnsureSetupPrereqsFor lets setup defer installing 1Password until after
// explicit packs have contributed inference. A pack using sbx-session auth
// must not make a keyless user install or sign into op.
func EnsureSetupPrereqsFor(env hostenv.Env, in io.Reader, out io.Writer, interactive, requireOp bool) error {
	var missing []string
	names := []string{"sbx"}
	if requireOp {
		names = append(names, "op")
	}
	for _, name := range names {

		if _, err := env.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	commands := map[string]string{
		"sbx": doctor.SbxInstallHint,
		"op":  "brew install 1password-cli",
	}
	fix := func() string {
		var lines []string
		for _, name := range missing {
			lines = append(lines, "  "+commands[name])
		}
		return strings.Join(lines, "\n")
	}
	if SetupHostOS != "darwin" {
		return fmt.Errorf("missing required host tool(s): %s\n%s", strings.Join(missing, ", "), fix())
	}
	if _, err := env.LookPath("brew"); err != nil {
		return fmt.Errorf("missing required host tool(s): %s; Homebrew is unavailable\n%s", strings.Join(missing, ", "), fix())
	}
	if !interactive || in == nil {
		return fmt.Errorf("missing required host tool(s): %s\n%s", strings.Join(missing, ", "), fix())
	}
	fmt.Fprintf(out, "Pix needs %s. Install with Homebrew now? [Y/n]: ", strings.Join(missing, " and "))
	if !cli.ConfirmYN(in, out, "", true) {
		return fmt.Errorf("required host tools were not installed\n%s", fix())
	}

	var formulae []string
	for _, name := range missing {
		switch name {
		case "sbx":
			formulae = append(formulae, "docker/tap/sbx@nightly")
		case "op":
			formulae = append(formulae, "1password-cli")
		}
	}
	if err := env.RunInteractive("brew", append([]string{"install"}, formulae...)...); err != nil {
		return fmt.Errorf("installing required host tools: %w", err)
	}
	for _, name := range missing {
		if _, err := env.LookPath(name); err != nil {
			return fmt.Errorf("Homebrew completed but %s is still not on PATH; restart the shell and re-run pix setup", name)
		}
	}
	return nil
}

// EnsureSetupSbxSession drives Docker's own login flow when the sbx control
// plane is not usable. It creates no Pix account and stores no Docker token.
func EnsureSetupSbxSession(env hostenv.Env, out io.Writer, interactive bool) error {
	if _, timedOut, err := env.RunTimed("sbx", "ls"); err == nil && !timedOut {
		return nil
	}
	if !interactive {
		return fmt.Errorf("Docker Sandboxes is not signed in or reachable; run: sbx login")
	}
	fmt.Fprintln(out, "Docker Sandboxes needs authorization. Continuing with the official `sbx login` flow.")
	if err := env.RunInteractive("sbx", "login"); err != nil {
		return fmt.Errorf("sbx login failed: %w", err)
	}
	if _, timedOut, err := env.RunTimed("sbx", "ls"); err != nil || timedOut {
		return fmt.Errorf("sbx login completed but Docker Sandboxes is still unreachable; run: sbx diagnose")
	}
	return nil
}

// EnsureSetupSbxDefaults owns the two one-time sbx settings Pix needs before
// its first sandbox can be created. It preserves an existing network policy and
// every existing kit publisher; setup only fills missing first-run state.
func EnsureSetupSbxDefaults(env hostenv.Env) error {
	if err := EnsureSetupKitAllowedSource(env); err != nil {
		return err
	}
	return EnsureSetupOpenNetworkPolicy(env)
}

func EnsureSetupKitAllowedSource(env hostenv.Env) error {
	out, timedOut, err := env.RunTimed("sbx", "settings", "get", setupKitSourcesKey)
	if err != nil || timedOut {
		return fmt.Errorf("reading Docker Sandboxes kit allowlist: %w", setupProbeError(err, timedOut))
	}
	var sources []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sources); err != nil {
		return fmt.Errorf("reading Docker Sandboxes kit allowlist: invalid JSON: %w", err)
	}
	for _, source := range sources {
		if source == "*" || source == setupKitAllowedSource {
			return nil
		}
	}
	sources = append(sources, setupKitAllowedSource)
	encoded, err := json.Marshal(sources)
	if err != nil {
		return fmt.Errorf("encoding Docker Sandboxes kit allowlist: %w", err)
	}
	if err := runSetupSbxCommand(env, "settings", "set", setupKitSourcesKey, string(encoded)); err != nil {
		return fmt.Errorf("allowing Pix's GitHub kit publisher: %w", err)
	}

	verified, timedOut, err := env.RunTimed("sbx", "settings", "get", setupKitSourcesKey)
	if err != nil || timedOut {
		return fmt.Errorf("verifying Docker Sandboxes kit allowlist: %w", setupProbeError(err, timedOut))
	}
	var after []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(verified)), &after); err != nil {
		return fmt.Errorf("verifying Docker Sandboxes kit allowlist: invalid JSON: %w", err)
	}
	for _, source := range after {
		if source == "*" || source == setupKitAllowedSource {
			return nil
		}
	}
	return fmt.Errorf("Docker Sandboxes did not retain Pix's kit publisher; run: sbx settings set %s '[\"docker.io/\",\"%s\"]'", setupKitSourcesKey, setupKitAllowedSource)
}

func EnsureSetupOpenNetworkPolicy(env hostenv.Env) error {
	initialized, inspectErr := SetupSbxNetworkPolicyInitialized(env)
	if initialized {
		return nil
	}
	if err := runSetupSbxCommand(env, "policy", "init", "allow-all"); err != nil {
		// Some sbx versions report uninitialized policy state as an error, while
		// an already-initialized daemon rejects a second init. Re-probe after a
		// rejected init so setup preserves an existing policy across both forms.
		if after, afterErr := SetupSbxNetworkPolicyInitialized(env); afterErr == nil && after {
			return nil
		}
		if inspectErr != nil {
			return fmt.Errorf("reading Docker Sandboxes network policy: %v; initializing it: %w", inspectErr, err)
		}
		return fmt.Errorf("initializing Docker Sandboxes network policy: %w", err)
	}
	initialized, err := SetupSbxNetworkPolicyInitialized(env)
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("Docker Sandboxes did not retain its network policy; run: sbx policy init allow-all")
	}
	return nil
}

func SetupSbxNetworkPolicyInitialized(env hostenv.Env) (bool, error) {
	out, timedOut, err := env.RunTimed("sbx", "policy", "ls", "--source", "local", "--type", "network", "--json")
	if err != nil || timedOut {
		return false, fmt.Errorf("reading Docker Sandboxes network policy: %w", setupProbeError(err, timedOut))
	}
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "{") {
		// Newer sbx releases wrap policy rows in a top-level object, while older
		// releases returned the rows as a bare array.
		var result struct {
			Rules json.RawMessage `json:"rules"`
		}
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			return false, fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: %w", err)
		}
		if len(result.Rules) == 0 {
			return false, fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: missing rules field")
		}
		var policies []json.RawMessage
		if err := json.Unmarshal(result.Rules, &policies); err != nil {
			return false, fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: %w", err)
		}
		return len(policies) > 0, nil
	}
	var policies []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &policies); err != nil {
		return false, fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: %w", err)
	}
	return len(policies) > 0, nil
}

func runSetupSbxCommand(env hostenv.Env, args ...string) error {
	return env.RunInteractiveQuiet("sbx", args...)
}

func setupProbeError(err error, timedOut bool) error {
	if timedOut {
		return fmt.Errorf("command timed out")
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("command failed")
}
