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
