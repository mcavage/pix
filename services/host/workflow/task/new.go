package task

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// NewOptions is the input to New.
type NewOptions struct {
	StateRoot string    // caller-resolved PIX_HOME task-state root
	Mainroot  string    // caller-resolved git-common-dir (see ResolveMainroot)
	StackID   string    // caller-resolved PIX_HOME stack identity
	Name      string    // task name (sanitized internally)
	Env       string    // resolved Pix environment selected at creation
	Ref       string    // start point; "" means HEAD
	Mechanism Mechanism // "" means Clone (the default)
}

// New creates a task's checkout + branch and writes its metadata. It does
// NOT launch a sandbox — that composition is the caller's job (see
// docs/design/worktree-tasks.md and cmd/pix/task_cmd.go, which hands the
// resulting checkout straight to the existing `pix run` path).
//
// On any failure after the checkout directory was reserved, New rolls the
// directory back (best-effort) so a failed `task new` never leaves a stray
// empty checkout behind.
func New(o NewOptions) (Meta, error) {
	if o.Mechanism == "" {
		o.Mechanism = Clone
	}
	if !o.Mechanism.Valid() {
		return Meta{}, fmt.Errorf("unknown mechanism %q", o.Mechanism)
	}
	sane := SanitizeName(o.Name)
	repokey := RepoKey(o.Mainroot)
	label := RepoLabel(o.Mainroot)
	sandboxName, err := ScopedSandboxName(o.StackID, label, repokey, o.Name)
	if err != nil {
		return Meta{}, err
	}
	repoDir := RepoDir(o.Mainroot)
	co, metaPath := Paths(o.StateRoot, repoDir, sane)

	owned, err := ReserveCheckout(co)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Meta{}, fmt.Errorf("task %q already exists; run `pix run --task %s` to reattach", o.Name, o.Name)
		}
		return Meta{}, fmt.Errorf("reserve checkout: %w", err)
	}
	rollback := func() {
		if owned {
			_ = os.RemoveAll(co)
		}
	}

	oid, err := ResolveCommit(o.Mainroot, o.Ref)
	if err != nil {
		rollback()
		return Meta{}, err
	}
	origin := OriginURL(o.Mainroot)
	branch := "pix/" + sane

	if err := createCheckout(o.Mechanism, o.Mainroot, co, oid, branch); err != nil {
		rollback()
		return Meta{}, err
	}
	if o.Mechanism == Clone && origin != "" {
		// Retarget origin to the real upstream (the clone's origin points at
		// mainroot's local path) so in-sandbox `git push`/`fetch` use it.
		_, _ = runIn(co, "remote", "set-url", "origin", origin)
	}

	meta := Meta{
		Name:      o.Name,
		Env:       o.Env,
		Mechanism: o.Mechanism,
		Sandbox:   sandboxName,
		Mainroot:  o.Mainroot,
		Branch:    branch,
		Base:      strings.TrimSpace(refOrHead(o.Ref)),
		Origin:    origin,
		Repo:      label,
		Created:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteMeta(metaPath, meta); err != nil {
		rollback()
		return Meta{}, fmt.Errorf("write metadata: %w", err)
	}
	return meta, nil
}

func refOrHead(ref string) string {
	if ref == "" {
		return "HEAD"
	}
	return ref
}
