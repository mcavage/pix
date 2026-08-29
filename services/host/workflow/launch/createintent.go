//go:build unix

// createintent.go — the bounded CREATE-INTENT state machine (P0-7): a fixed,
// small, allowlisted record written to disk atomically and symlink-safe
// BEFORE this process ever spawns `sbx env create`, and replaced by the
// instance-bound lease.Record (see lease/record.go's CreateRecordFor) only
// once a caller has a VERIFIED POSITIVE create receipt — never on a bare
// probe, never speculatively.
//
// The whole point of writing the intent FIRST is the crash window it closes:
// if this process (or the host) dies between "sbx env create was asked to
// run" and "the receipt was confirmed", nothing else on this host has any
// record that a create was ever attempted. Without the intent, a next run or
// the orphan sweep sees nothing at all and cannot tell "nothing happened"
// from "something may have been left behind". With it, DiagnoseResidue can
// say exactly that.
//
// Cleanup authority is the other half, and it is deliberately narrow:
// DecideEnvRemoval only ever authorizes `sbx env rm -f` when BOTH a positive
// receipt exists AND a FRESH probe (taken right before the decision, never
// one cached from create time) reports the EXACT SAME instance id and name
// the receipt names. A pre-create absent probe is never, by itself, removal
// authority — absence of evidence is not evidence of a confirmed create. No
// receipt at all means zero `sbx` remove invocations, ever: this package
// reports possible residue instead and leaves the sandbox (if any) alone for
// a human or a later, better-informed decision.
//
// No secret, token, or credential value is ever a field on CreateIntent: only
// identity and naming facts (environment root, sandbox name, the fingerprint
// this create is FOR) that a next run or the orphan sweep needs to diagnose
// partial state. The field set is fixed and allowlisted — see
// createintent_test.go's json-key assertion — not an open bag a future
// caller could quietly widen into carrying something sensitive.
package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// CreateIntentFileName is the bounded record's file name inside a lease
// sandbox directory, sibling to lease's own record.json/fingerprint.json.
const CreateIntentFileName = "createintent.json"

// CreateIntent is what this launcher commits to disk BEFORE it ever spawns
// `sbx env create`: the environment's canonical identity (root), the
// sandbox name being created, and the fingerprint this create is FOR.
// Nothing else — no secret, no token, no credential value.
type CreateIntent struct {
	// EnvironmentRoot is the environment's canonical, already-absolute root
	// (the same identity workflow/env.Subject keys acceptance records by) —
	// the CALLER is responsible for canonicalizing it before constructing a
	// CreateIntent; this package validates only that it looks canonical
	// (non-empty, absolute), it does not canonicalize on its own.
	EnvironmentRoot string `json:"environment_root"`
	// EnvironmentName is the registered name this create resolved
	// EnvironmentRoot from, kept for a human reading the file — never used
	// for identity comparison (the root is).
	EnvironmentName string `json:"environment_name,omitempty"`
	// SandboxName is the pix-* name this create targets.
	SandboxName string `json:"sandbox_name"`
	// Fingerprint is the desired creation fingerprint this create is FOR —
	// the same sandbox.Fingerprint shape the session lifecycle already
	// records and diffs.
	Fingerprint sandbox.Fingerprint `json:"fingerprint,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	CreatedPID  int                 `json:"created_pid"`
}

func validateCreateIntent(intent CreateIntent) error {
	if strings.TrimSpace(intent.EnvironmentRoot) == "" {
		return fmt.Errorf("launch: create intent requires a non-empty environment root")
	}
	if !filepath.IsAbs(intent.EnvironmentRoot) {
		return fmt.Errorf("launch: create intent environment root %q is not an absolute canonical path", intent.EnvironmentRoot)
	}
	if intent.SandboxName == "" {
		return fmt.Errorf("launch: create intent requires a sandbox name")
	}
	if !strings.HasPrefix(intent.SandboxName, sandbox.Prefix) {
		return fmt.Errorf("launch: create intent sandbox name %q is outside the %s* namespace", intent.SandboxName, sandbox.Prefix)
	}
	return nil
}

// refuseSymlinkAt refuses when a real filesystem entry already exists at
// path AND is a symlink. A missing path is not an error here.
func refuseSymlinkAt(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("launch: refusing to follow symlink at %s", path)
	}
	return fi, nil
}

// WriteCreateIntent writes intent to dir's create-intent record BEFORE the
// caller spawns `sbx env create`. It is:
//
//   - ATOMIC: same-directory temp file plus rename (writeFileAtomic), so a
//     reader never observes a half-written file, and a crash mid-write
//     leaves either the old content or nothing new — never a torn mix.
//   - SYMLINK-SAFE: dir itself is refused if it is (or resolves through) a
//     symlink (lease.EnsureSandboxDir), and an existing createintent.json
//     that is already a symlink is refused rather than written through —
//     rename(2) never follows the final path component either way, but this
//     package refuses outright rather than silently replacing a symlink
//     some other process may be relying on.
//
// Unlike lease.CreateRecord, an intent is NOT write-once: a create attempt
// for the same key may be retried, and each attempt overwrites the prior
// intent with a fresh one. What makes it safe to overwrite is exactly what
// makes lease.Record safe to never overwrite: the intent is never itself
// removal authority, only a receipt (a promoted lease.Record) is.
func WriteCreateIntent(dir string, intent CreateIntent) error {
	if err := validateCreateIntent(intent); err != nil {
		return err
	}
	if err := lease.EnsureSandboxDir(dir); err != nil {
		return err
	}
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = time.Now().UTC()
	}
	if intent.CreatedPID == 0 {
		intent.CreatedPID = os.Getpid()
	}
	path := filepath.Join(dir, CreateIntentFileName)
	if _, err := refuseSymlinkAt(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("launch: marshal create intent: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("launch: write create intent %s: %w", path, err)
	}
	return nil
}

// ReadCreateIntent reads dir's create-intent record. found is false (with a
// nil error) when nothing was ever written there — the ordinary shape once
// PromoteCreateIntent has cleared it, or for a directory that never started
// a create at all.
func ReadCreateIntent(dir string) (intent *CreateIntent, found bool, err error) {
	path := filepath.Join(dir, CreateIntentFileName)
	if _, err := refuseSymlinkAt(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	var v CreateIntent
	if uerr := json.Unmarshal(data, &v); uerr != nil {
		return nil, false, fmt.Errorf("launch: corrupt create intent at %s: %w", path, uerr)
	}
	return &v, true, nil
}

// ClearCreateIntent removes dir's create-intent record (and any crash-
// leftover writeFileAtomic temp file beside it). Removing an already-absent
// intent is not an error.
func ClearCreateIntent(dir string) error {
	path := filepath.Join(dir, CreateIntentFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	leftover, _ := filepath.Glob(path + ".tmp-*")
	for _, p := range leftover {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// CreateReceipt is the VERIFIED POSITIVE outcome of a create: the instance id
// and sandbox name a caller confirmed sbx actually created. The only
// producer of a durable one is PromoteCreateIntent (via
// lease.CreateRecordFor) — nothing in this package invents a receipt from a
// probe alone.
type CreateReceipt struct {
	InstanceID  string
	SandboxName string
}

// PromoteCreateIntent replaces dir's create-intent with the instance-bound
// lease record on a verified positive create receipt: it creates the
// (immutable, write-once) lease.Record for receipt naming BOTH the instance
// id and the sandbox name, and only THEN clears the create-intent file — in
// that order, so a crash between the two still leaves the lease record (the
// stronger proof) on disk; it can never leave neither, and it can never
// leave a cleared intent with no record to replace it.
func PromoteCreateIntent(dir string, receipt CreateReceipt) (*lease.Record, error) {
	if receipt.InstanceID == "" {
		return nil, fmt.Errorf("launch: cannot promote a create intent with no receipt instance id")
	}
	if receipt.SandboxName == "" {
		return nil, fmt.Errorf("launch: cannot promote a create intent with no receipt sandbox name")
	}
	rec, err := lease.CreateRecordFor(dir, receipt.InstanceID, receipt.SandboxName)
	if err != nil {
		return nil, err
	}
	if err := ClearCreateIntent(dir); err != nil {
		return rec, fmt.Errorf("launch: promoted %s but could not clear its create intent: %w", receipt.InstanceID, err)
	}
	return rec, nil
}

// LoadCreateReceipt reads dir's lease.Record — the ONLY thing that ever
// counts as a "verified positive create receipt" (the one thing
// PromoteCreateIntent ever writes) — and reports the receipt it names, or
// nil when no record exists: creation was never confirmed here.
func LoadCreateReceipt(dir string) *CreateReceipt {
	rec, err := lease.ReadRecord(dir)
	if err != nil {
		return nil
	}
	return &CreateReceipt{InstanceID: rec.InstanceID, SandboxName: rec.Name}
}

// DiagnoseResidue reads dir's create-intent and lease record and reports
// whether a NEXT RUN or the orphan sweep should treat dir as possible
// residue from an interrupted or failed create: an intent exists with NO
// promoted receipt. It runs no probe of its own and performs no removal —
// diagnosis only, for a caller (or a human) to decide what to do next.
func DiagnoseResidue(dir string) (report string, residue bool, err error) {
	intent, found, ierr := ReadCreateIntent(dir)
	if ierr != nil {
		return "", false, ierr
	}
	if !found {
		return "", false, nil
	}
	if LoadCreateReceipt(dir) != nil {
		// A receipt exists — the create IS confirmed — but the intent file
		// itself survived (a crash between the record write and the intent
		// clear inside PromoteCreateIntent). Not residue: only cleanup of
		// the now-redundant intent file is outstanding.
		return "", false, nil
	}
	return fmt.Sprintf(
		"create intent for sandbox %q (environment %s) was written at %s but was never confirmed by a receipt; it may be residue from an interrupted or failed create — verify with `sbx ls` before creating again",
		intent.SandboxName, intent.EnvironmentRoot, intent.CreatedAt.Format(time.RFC3339)), true, nil
}

// EnvRemovalVerdict is what DecideEnvRemoval decided, as a stable string for
// logging and tests — the same "never guess" posture reap.go's
// TeardownVerdict holds for the pix-* name-based lifecycle, applied here to
// the create-intent/receipt state machine.
type EnvRemovalVerdict string

const (
	// EnvRemovalAuthorized: a positive create receipt exists AND a fresh,
	// trusted probe reports the EXACT SAME instance id and name it names.
	// `sbx env rm -f` may run.
	EnvRemovalAuthorized EnvRemovalVerdict = "authorized"
	// EnvRemovalNoReceipt: creation was never confirmed — no receipt at
	// all, whether or not an intent (or nothing) is on disk. Zero rm
	// invocations, ever; report possible residue instead.
	EnvRemovalNoReceipt EnvRemovalVerdict = "no-receipt"
	// EnvRemovalStale: the receipt exists, but a fresh probe's instance id
	// or name does not match it — the name was reused for a different
	// instance, or moved.
	EnvRemovalStale EnvRemovalVerdict = "stale"
	// EnvRemovalUnknownProbe: the fresh probe could not be trusted (not
	// run, timed out, schema-unverified, or missing an instance id). Fail
	// closed: never treated as either a match or a mismatch.
	EnvRemovalUnknownProbe EnvRemovalVerdict = "unknown-probe"
)

// EnvRemovalInput is everything DecideEnvRemoval needs. This package never
// runs sbx itself — Probe must already be the result of a probe taken RIGHT
// BEFORE this decision, never one cached from create time or from an earlier
// decision.
type EnvRemovalInput struct {
	// Receipt is the confirmed creation identity (LoadCreateReceipt), or nil
	// when creation was never positively confirmed for this directory.
	Receipt *CreateReceipt
	// Intent is read only for diagnostic/residue reporting: it is NEVER
	// itself removal authority, with or without a matching probe.
	Intent *CreateIntent
	// ProbeTrusted reports whether the fresh probe could be trusted at all
	// (e.g. `sbx ls --json` ran, didn't time out, and parsed under
	// sandbox.ParseResult.SchemaVerified). false regardless of Probe's
	// content.
	ProbeTrusted bool
	// Probe is the fresh listing row for the receipt's sandbox name, or nil
	// when the probe was trusted but found no such row (a positively absent
	// sandbox — not, on its own, removal authority for anything: there is
	// nothing left to remove).
	Probe *sandbox.Entry
}

// DecideEnvRemoval is the WHOLE removal-authority state machine, and it is
// the ONLY function in this package that may answer "is `sbx env rm -f`
// authorized". It never runs sbx and never mutates anything.
//
// A pre-create absent probe is NEVER, by itself, removal authority: with no
// Receipt, every branch below returns EnvRemovalNoReceipt regardless of what
// Probe/ProbeTrusted say — absence of evidence that something was ever
// created is not evidence that removal is safe, it is evidence that removal
// has nothing to prove itself against.
func DecideEnvRemoval(input EnvRemovalInput) (EnvRemovalVerdict, string) {
	if input.Receipt == nil || input.Receipt.InstanceID == "" || input.Receipt.SandboxName == "" {
		detail := "no positive create receipt was ever recorded for this create; refusing to remove"
		if input.Intent != nil {
			detail += fmt.Sprintf(" (an unconfirmed create intent for %q is still on disk — this may be residue from an interrupted or failed create)", input.Intent.SandboxName)
		}
		return EnvRemovalNoReceipt, detail
	}
	if !input.ProbeTrusted {
		return EnvRemovalUnknownProbe, fmt.Sprintf("could not trust a fresh probe to confirm instance %q (%q) is still what is running; refusing to remove", input.Receipt.InstanceID, input.Receipt.SandboxName)
	}
	if input.Probe == nil {
		return EnvRemovalUnknownProbe, fmt.Sprintf("a fresh, trusted probe found no sandbox named %q at all; refusing to remove a receipted instance %q it cannot re-confirm", input.Receipt.SandboxName, input.Receipt.InstanceID)
	}
	if !input.Probe.IdentityVerified || input.Probe.InstanceID == nil || *input.Probe.InstanceID == "" {
		return EnvRemovalUnknownProbe, fmt.Sprintf("fresh probe of %q did not positively identify an instance id; refusing to remove the receipted %q", input.Receipt.SandboxName, input.Receipt.InstanceID)
	}
	if input.Probe.Name != input.Receipt.SandboxName {
		return EnvRemovalStale, fmt.Sprintf("fresh probe named %q, not the receipted %q — refusing to remove", input.Probe.Name, input.Receipt.SandboxName)
	}
	if *input.Probe.InstanceID != input.Receipt.InstanceID {
		return EnvRemovalStale, fmt.Sprintf("%q now runs instance %q, not the receipted %q — the name was reused; refusing to remove", input.Receipt.SandboxName, *input.Probe.InstanceID, input.Receipt.InstanceID)
	}
	return EnvRemovalAuthorized, fmt.Sprintf("receipt and fresh probe both agree: %q is instance %q", input.Receipt.SandboxName, input.Receipt.InstanceID)
}

// envRmArgv is the exact argv this package's cleanup authority composes for
// a positively-authorized removal. It is private and used only through
// CleanupEnv, which is the ONE path that may call it — so "zero rm
// invocations" (every non-authorized verdict) and "exact argv" (the one
// authorized verdict) are the same property, tested from both directions.
func envRmArgv(name string) []string {
	return []string{"env", "rm", "-f", name}
}

// CleanupEnv is DecideEnvRemoval plus the single call it may authorize.
// remove is invoked AT MOST ONCE, and ONLY when the verdict is
// EnvRemovalAuthorized — every other verdict returns without calling remove
// at all, so a caller wiring remove to a real `sbx` invocation gets zero
// invocations for every refusal, by construction rather than by convention.
func CleanupEnv(input EnvRemovalInput, remove func(argv []string) error) (EnvRemovalVerdict, string, error) {
	verdict, detail := DecideEnvRemoval(input)
	if verdict != EnvRemovalAuthorized {
		return verdict, detail, nil
	}
	if err := remove(envRmArgv(input.Receipt.SandboxName)); err != nil {
		return verdict, detail, err
	}
	return verdict, detail, nil
}
