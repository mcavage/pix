package provision

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"pix/host/hostenv"
)

const (
	setupKitAllowedSource = "github.com/mcavage/"
	setupKitSourcesKey    = "kit.allowedSources"
)

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
	if err := ensureKitAllowedSource(env); err != nil {
		return err
	}
	return ensureOpenNetworkPolicy(env)
}

// kitAllowedSources reads sbx's kit publisher allowlist and reports whether it
// already carries Pix's publisher. ONE reader, shared by the decision and the
// verify-after-write, so the allowlist's shape is parsed in exactly one place.
func kitAllowedSources(env hostenv.Env, stage string) ([]string, bool, error) {
	out, timedOut, err := env.RunTimed("sbx", "settings", "get", setupKitSourcesKey)
	if err != nil || timedOut {
		return nil, false, fmt.Errorf("%s Docker Sandboxes kit allowlist: %w", stage, setupProbeError(err, timedOut))
	}
	var sources []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sources); err != nil {
		return nil, false, fmt.Errorf("%s Docker Sandboxes kit allowlist: invalid JSON: %w", stage, err)
	}
	for _, source := range sources {
		if source == "*" || source == setupKitAllowedSource {
			return sources, true, nil
		}
	}
	return sources, false, nil
}

func ensureKitAllowedSource(env hostenv.Env) error {
	sources, allowed, err := kitAllowedSources(env, "reading")
	if err != nil || allowed {
		return err
	}
	encoded, err := json.Marshal(append(sources, setupKitAllowedSource))
	if err != nil {
		return fmt.Errorf("encoding Docker Sandboxes kit allowlist: %w", err)
	}
	if err := env.RunInteractiveQuiet("sbx", "settings", "set", setupKitSourcesKey, string(encoded)); err != nil {
		return fmt.Errorf("allowing Pix's GitHub kit publisher: %w", err)
	}
	// Verified, never assumed: sbx keeping the write is a probe, not a promise.
	if _, allowed, err = kitAllowedSources(env, "verifying"); err != nil {
		return err
	} else if !allowed {
		return fmt.Errorf("Docker Sandboxes did not retain Pix's kit publisher; run: sbx settings set %s '[\"docker.io/\",\"%s\"]'", setupKitSourcesKey, setupKitAllowedSource)
	}
	return nil
}

func ensureOpenNetworkPolicy(env hostenv.Env) error {
	initialized, inspectErr := sbxNetworkPolicyInitialized(env)
	if initialized {
		return nil
	}
	if err := env.RunInteractiveQuiet("sbx", "policy", "init", "allow-all"); err != nil {
		// Some sbx versions report uninitialized policy state as an error, while
		// an already-initialized daemon rejects a second init. Re-probe after a
		// rejected init so setup preserves an existing policy across both forms.
		if after, afterErr := sbxNetworkPolicyInitialized(env); afterErr == nil && after {
			return nil
		}
		if inspectErr != nil {
			return fmt.Errorf("reading Docker Sandboxes network policy: %v; initializing it: %w", inspectErr, err)
		}
		return fmt.Errorf("initializing Docker Sandboxes network policy: %w", err)
	}
	initialized, err := sbxNetworkPolicyInitialized(env)
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("Docker Sandboxes did not retain its network policy; run: sbx policy init allow-all")
	}
	return nil
}

func sbxNetworkPolicyInitialized(env hostenv.Env) (bool, error) {
	out, timedOut, err := env.RunTimed("sbx", "policy", "ls", "--source", "local", "--type", "network", "--json")
	if err != nil || timedOut {
		return false, fmt.Errorf("reading Docker Sandboxes network policy: %w", setupProbeError(err, timedOut))
	}
	trimmed := strings.TrimSpace(out)
	rows := json.RawMessage(trimmed)
	if strings.HasPrefix(trimmed, "{") {
		// Newer sbx releases wrap policy rows in a top-level object, while older
		// releases returned the rows as a bare array.
		var result struct {
			Rules json.RawMessage `json:"rules"`
		}
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			return false, policyJSONError(err)
		}
		if len(result.Rules) == 0 {
			return false, fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: missing rules field")
		}
		rows = result.Rules
	}
	var policies []json.RawMessage
	if err := json.Unmarshal(rows, &policies); err != nil {
		return false, policyJSONError(err)
	}
	return len(policies) > 0, nil
}

func policyJSONError(err error) error {
	return fmt.Errorf("reading Docker Sandboxes network policy: invalid JSON: %w", err)
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
