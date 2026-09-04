package health

import (
	"context"
	"fmt"
	"strings"
)

// github.go — the doctor row for a sandbox's GitHub credential.
//
// It exists because the failure was invisible until the worst moment. A sandbox
// has no GitHub credential of its own, so an agent commits, finishes, tries to
// push, and only then reports that it cannot. Nothing in setup or doctor said a
// token was needed.

// GitHubSecretProbe reports whether every sandbox can reach GitHub.
type GitHubSecretProbe struct {
	// Scope answers the question. It is injected so the row can be tested
	// without sbx: state, plus the sandboxes a scoped secret is pinned to.
	Scope func() (state int, sandboxes []string)
	// Fix is the command that repairs both failing cases.
	Fix string
}

// The states, mirroring secret.GitHubSecretState. Duplicated as ints rather
// than imported because health must not depend on secret: the composition root
// supplies Scope, exactly as it supplies every other capability seam here.
const (
	GitHubUnknown = iota
	GitHubGlobal
	GitHubScoped
	GitHubAbsent
)

func (GitHubSecretProbe) Name() string { return "github" }

// Required is false. Plenty of real work never pushes, so a missing token must
// be VISIBLE without failing the exit code of every script that runs doctor.
func (GitHubSecretProbe) Required() bool { return false }

// Check never answers StatusOff. Both non-ready states below are pix's own
// INFERENCE from the outside (sbx has no global secret, or has one pinned to
// the wrong scope) rather than a choice the user made about pushing — pix has
// no "I opted out of pushing" setting to point at, and the failure this row
// exists to catch lands only after an agent has already committed. The
// eligibility rule for StatusOff (see health.go) requires exactly that kind of
// user-owned setting; this probe has none, so it stays absent, keeps its Fix,
// and keeps asking to be repaired rather than being trusted quietly forever.
func (p GitHubSecretProbe) Check(context.Context) Result {
	if p.Scope == nil {
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "not wired",
			Evidence: "no scope resolver was supplied"}
	}
	state, boxes := p.Scope()
	switch state {
	case GitHubGlobal:
		return Result{Name: p.Name(), Status: StatusReady, Detail: "every sandbox can push",
			Evidence: "a global github service secret is set"}
	case GitHubScoped:
		// STORED, and still a gap. The secret works for the boxes named and for
		// no others, including every sandbox created from now on, which is the
		// shape that makes this worth a row of its own.
		return Result{Name: p.Name(), Status: StatusAbsent, Fix: p.Fix,
			Detail: fmt.Sprintf("scoped to %d sandbox(es), not global", len(boxes)),
			Evidence: "github is stored only for " + strings.Join(boxes, ", ") +
				"; a sandbox created later gets no credential"}
	case GitHubAbsent:
		return Result{Name: p.Name(), Status: StatusAbsent, Fix: p.Fix,
			Detail:   "no github secret",
			Evidence: "sbx has no github service secret, so a sandbox can commit but not push"}
	default:
		// sbx could not answer. Never a gap: a probe that did not run is not
		// evidence that a credential is missing.
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "could not check",
			Evidence: "sbx did not answer `secret ls`"}
	}
}

// IgnoredGlobalSecretsProbe reports GLOBAL sbx secrets this host holds that
// Pix does not use. It exists because the state is invisible and confusing:
// `sbx secret ls` shows an anthropic key, the agent still cannot reach a
// model, and nothing explains that Pix reads only its own refs and writes only
// sandbox-scoped secrets. This row says so, names the optional removal
// command, and removes nothing itself.
type IgnoredGlobalSecretsProbe struct {
	// Scan enumerates the global provider/github secret NAMES. known is
	// false when sbx could not be asked: an enumeration that did not happen
	// is reported as unknown, never as "there are none".
	Scan func() (names []string, known bool)
	// Fix is the exact, OPTIONAL manual removal command.
	Fix string
}

func (IgnoredGlobalSecretsProbe) Name() string   { return "sbx-globals" }
func (IgnoredGlobalSecretsProbe) Required() bool { return false }

func (p IgnoredGlobalSecretsProbe) Check(context.Context) Result {
	if p.Scan == nil {
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "not wired",
			Evidence: "no global-secret scanner was supplied"}
	}
	names, known := p.Scan()
	if !known {
		return Result{Name: p.Name(), Status: StatusUnknown, Detail: "could not check",
			Evidence: "sbx did not answer `secret ls --global`"}
	}
	if len(names) == 0 {
		return Result{Name: p.Name(), Status: StatusReady, Detail: "no ignored global secrets",
			Evidence: "sbx holds no global provider or github service secret"}
	}
	return Result{Name: p.Name(), Status: StatusAbsent, Fix: p.Fix,
		Detail: fmt.Sprintf("%d global sbx secret(s) Pix ignores: %s", len(names), strings.Join(names, ", ")),
		Evidence: "Pix resolves its own op:// refs into each sandbox and never reads a host-global secret; " +
			"these belong to whatever put them there and are left alone"}
}
