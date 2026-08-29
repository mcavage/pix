// review.go — E1.8's `pix env review` trust gate: docs/design/
// environments.md §9, PRD §5.8 (AC-66), the environment analog of
// workflow/pack's Tier-1 packTrustGate (trust.go). It renders the host bill
// of materials (bom.go), gates on explicit consent, and — only on
// acceptance — records it under hosttrust, in launcher-owned state OUTSIDE
// the environment's own payload (never inside the registered root, which is
// as attacker-controlled for a cloned/shared environment as a pack payload
// is).
//
// # TOCTOU
//
// Review NEVER accepts a caller-held *Environment: it is the sole entry
// point that loads one, and it loads TWICE — once to render the bill a
// human (or --yes) consents to, and once more, under the SAME lock as the
// store write, immediately before computing the fingerprint that actually
// gets persisted. A caller cannot hand Review a stale snapshot because
// Review never accepts one in the first place, and nothing that changes on
// disk between "what was shown" and "what gets stored" can sneak an
// unreviewed surface into the trust store: the second Load re-runs every
// refusal (symlinked root/reference, containment, strict parse) exactly as
// the first one did, so a mutation introduced during an interactive
// prompt's wait fails the SAME way a first-time Load would.
package env

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

// ── launcher-owned trust state ──────────────────────────────────────────

const environmentTrustStoreName = "environment-trust.json"

// environmentTrustStore is the ONE acceptance document every environment's
// host-exec review is recorded in — never inside pix.toml or .sbxenv.yaml,
// which sit in the environment's own (possibly shared/cloned) directory.
// hosttrust.AcceptanceStore is embedded, not re-implemented (hosttrust's
// own doc.go: "a future subject kind... reuses this exact type rather than
// growing a parallel record shape" — F6 of the pack extraction), so an
// environment's Record is byte-shape-identical to a pack's.
type environmentTrustStore struct {
	Version int `json:"version"`
	hosttrust.AcceptanceStore
}

func environmentTrustStorePath() string {
	return filepath.Join(filepath.Dir(config.Path()), environmentTrustStoreName)
}

// environmentTrustLockPath lives in the STATE dir, never beside the store in
// the config dir — the same reasoning packTrustLockPath documents: moving
// the config dir aside must never orphan a held lock.
func environmentTrustLockPath() string {
	dir, err := config.StateDir()
	if err != nil {
		return filepath.Join(filepath.Dir(config.Path()), "environment-trust.lock")
	}
	return filepath.Join(dir, "environment-trust.lock")
}

// loadEnvironmentTrustStore reads the trust store fresh. Absent -> an empty
// store (fresh host, nothing accepted). Unreadable/unparsable is an ERROR,
// never a partial decode — the same fail-closed contract
// loadPackTrustStore already holds itself to.
func loadEnvironmentTrustStore() (*environmentTrustStore, error) {
	b, err := hosttrust.ReadDocumentBytes(environmentTrustStorePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &environmentTrustStore{Version: 1}, nil
		}
		return nil, err
	}
	var s environmentTrustStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", environmentTrustStorePath(), err)
	}
	return &s, nil
}

// Save writes the store symlink-safe + atomic (hosttrust.SaveDocument).
func (s *environmentTrustStore) Save() error {
	s.Version = 1
	return hosttrust.SaveDocument(filepath.Dir(config.Path()), environmentTrustStoreName, s)
}

// withEnvironmentTrustLock runs fn holding the exclusive cross-process flock
// serializing every environment-trust read-modify-write. Single lock, never
// nested — flock is per open file description (hosttrust.WithLock's own doc
// comment).
func withEnvironmentTrustLock(fn func() error) error {
	return hosttrust.WithLock(environmentTrustLockPath(), fn)
}

// mutateEnvironmentTrustStoreLocked is the sanctioned write path: under the
// lock, fresh load -> mutate -> save (hosttrust.LoadMutateSave), so no
// caller can commit a stale in-memory store over a concurrent writer's
// record.
func mutateEnvironmentTrustStoreLocked(mutate func(*environmentTrustStore) error) (*environmentTrustStore, error) {
	var fresh *environmentTrustStore
	err := withEnvironmentTrustLock(func() error {
		var e error
		fresh, e = hosttrust.LoadMutateSave(loadEnvironmentTrustStore, mutate, func(s *environmentTrustStore) error { return s.Save() })
		return e
	})
	return fresh, err
}

// ── rendering (PRD §5.8 / AC-66) ─────────────────────────────────────────

// labelColumnWidth is the fixed count-label field width every §5.8 review
// line pads to: "  " (2) + label (ljust 20) + " " (1) = the value column
// starting at byte 23 on every line, continuation lines included. Pinned by
// bom_test.go's byte-exact golden against the PRD fixture — do not "clean
// up" this magic number without re-deriving it from §5.8's own text.
const labelColumnWidth = 20

// credentialSourceColumnWidth is the fixed width a credential target's
// Source is left-padded to before "-> destination", so multiple
// credential-target lines' arrows visually align — see PRD §5.8's own
// two-line example.
const credentialSourceColumnWidth = 32

func pluralize(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	plural := singular + "s"
	if strings.HasSuffix(singular, "y") {
		plural = strings.TrimSuffix(singular, "y") + "ies"
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// writeCountLine renders one §5.8 count line: the label and the first value
// share a line; every additional value gets its own continuation line,
// indented to the SAME value column (an empty label, padded identically).
func writeCountLine(out io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(out, "  %-*s %s\n", labelColumnWidth, label, values[0])
	for _, v := range values[1:] {
		fmt.Fprintf(out, "  %-*s %s\n", labelColumnWidth, "", v)
	}
}

// renderCounts renders every non-empty BillOfMaterials category as one
// §5.8 count line, in the fixed order the PRD fixture itself uses: host
// commands, host service, credential targets, new mounts, then the two
// facets §5.8's own example fixture happens not to exercise (no-verify
// registries, interpolations) — present only when non-empty, so an
// environment with neither renders byte-identical to the PRD example.
func renderCounts(out io.Writer, b BillOfMaterials) {
	if n := len(b.HostCommands); n > 0 {
		names := make([]string, n)
		for i, c := range b.HostCommands {
			names[i] = c.Name
		}
		writeCountLine(out, pluralize(n, "host command"), []string{strings.Join(names, ", ")})
	}
	if n := len(b.HostServices); n > 0 {
		vals := make([]string, n)
		for i, s := range b.HostServices {
			vals[i] = fmt.Sprintf("%s  port %d", s.Name, s.Port)
		}
		writeCountLine(out, pluralize(n, "host service"), vals)
	}
	if n := len(b.CredentialTargets); n > 0 {
		vals := make([]string, n)
		for i, c := range b.CredentialTargets {
			vals[i] = fmt.Sprintf("%-*s-> %s", credentialSourceColumnWidth, c.Source, c.Destination)
		}
		writeCountLine(out, pluralize(n, "credential target"), vals)
	}
	if n := len(b.EffectiveMounts); n > 0 {
		vals := make([]string, n)
		for i, m := range b.EffectiveMounts {
			ro := "rw"
			if m.ReadOnly {
				ro = "ro"
			}
			vals[i] = fmt.Sprintf("%s   (%s)", m.Path, ro)
		}
		writeCountLine(out, pluralize(n, "new mount"), vals)
	}
	if nv := b.NoVerifyRegistries(); len(nv) > 0 {
		vals := make([]string, len(nv))
		for i, r := range nv {
			vals[i] = r.Host
		}
		writeCountLine(out, pluralize(len(nv), "no-verify registry"), vals)
	}
	if n := len(b.Interpolations); n > 0 {
		vals := make([]string, n)
		for i, it := range b.Interpolations {
			vals[i] = fmt.Sprintf("%s -> %s", interpolationSource(it.Var, it.Default), it.KeyPath)
		}
		writeCountLine(out, pluralize(n, "interpolation"), vals)
	}
}

// interpolationSource renders one authored `${VAR}` (or `${VAR:-default}`)
// reference in its ORIGINAL authored form — never a resolved value; Default
// is authored file content, not anything read from the process environment.
func interpolationSource(v string, def *string) string {
	if def != nil {
		return fmt.Sprintf("${%s:-%s}", v, *def)
	}
	return fmt.Sprintf("${%s}", v)
}

// renderVerboseDetails is `--verbose`'s addition (AC-66): full argv for
// every host command and host service, and the content digest for every
// local kit and resolved host service executable.
func renderVerboseDetails(out io.Writer, b BillOfMaterials) {
	for _, c := range b.HostCommands {
		fmt.Fprintf(out, "  host command %-10s argv: %s\n", c.Name, strings.Join(c.Argv, " "))
	}
	for _, s := range b.HostServices {
		line := s.Command
		if len(s.Args) > 0 {
			line += " " + strings.Join(s.Args, " ")
		}
		fmt.Fprintf(out, "  host service %-10s argv: %s\n", s.Name, line)
		if s.SHA != "" {
			fmt.Fprintf(out, "                            sha256:%s\n", s.SHA)
		}
	}
	for _, k := range b.Kits {
		if !k.Local {
			continue
		}
		fmt.Fprintf(out, "  kit %-16s path: %s\n", k.Raw, k.Resolved)
		fmt.Fprintf(out, "                      sha256:%s\n", k.SHA)
	}
}

// renderBill renders the complete §5.8 review screen: header, counts,
// either the `--verbose` tip (default tier) or the full detail block
// (verbose tier), and the consent prompt itself — the SAME text whether the
// caller is about to read a real answer (TTY) or about to fail closed
// because it cannot (non-TTY): "prints the same bill" (§5.8) is true by
// construction because both paths call this one function.
func renderBill(out io.Writer, name string, b BillOfMaterials, verbose bool) {
	fmt.Fprintf(out, "Environment %q runs code on your host and hands it credentials.\n\n", name)
	renderCounts(out, b)
	fmt.Fprintln(out)
	if verbose {
		renderVerboseDetails(out, b)
		fmt.Fprintln(out)
	} else {
		fmt.Fprintf(out, "  full argv and content digests: pix env review %s --verbose\n\n", name)
	}
	fmt.Fprint(out, "Accept this host-execution footprint? [y/N]:")
}

// ── the gate ──────────────────────────────────────────────────────────────

// gate renders the bill and requires explicit consent. yes accepts outright
// (the bill still renders, for the record). A non-TTY without yes prints
// the SAME bill plus the exact `--yes` re-run command and fails closed as a
// cli.UsageError (exit 2) — nothing is written. On a TTY the default answer
// is No: EOF, a blank line, or anything but y/yes refuses.
func gate(in io.Reader, out io.Writer, tty, yes bool, name string, b BillOfMaterials, verbose bool) error {
	renderBill(out, name, b, verbose)
	if yes {
		fmt.Fprintln(out, "\naccepted via --yes")
		return nil
	}
	if !tty || in == nil {
		fmt.Fprintf(out, "\npix env review %s --yes\n", name)
		return cli.UsageError{Err: fmt.Errorf(
			"environment %q would run the above on your host; refusing to review it non-interactively (fail closed)", name,
		)}
	}
	sc := bufio.NewScanner(in)
	fmt.Fprintln(out)
	if !sc.Scan() {
		return fmt.Errorf("environment %q not accepted (no answer; default is No)", name)
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("environment %q not accepted (you said no)", name)
}

// ── Review: the composed entry point ─────────────────────────────────────

// ReviewOptions groups review's I/O and mode — everything a future `pix env
// review` command line parses out of its flags and its own stdin/stdout.
type ReviewOptions struct {
	Verbose bool
	Yes     bool
	TTY     bool
	In      io.Reader
	Out     io.Writer
}

// ReviewResult is what Review did, for a caller to report. Fingerprint is
// "" for a Tier0 environment (nothing was computed worth naming).
type ReviewResult struct {
	Accepted    bool
	Fingerprint string
}

// Review is E1.8's composed entry point: load the environment (never a
// caller-supplied one — see this file's TOCTOU doc comment), compute its
// bill of materials, and — for a Tier0 (non-host-executing) environment —
// return accepted with NO output and NO store write at all: there is
// nothing to review. For a Tier1 environment, render the bill, gate on
// explicit consent (opts), and on acceptance reload+recompute ONE more
// time, under the same lock as the store write, before persisting the
// record under hosttrust — outside the environment's own payload, keyed by
// Subject(root), never by name (a repoint can never inherit acceptance:
// AC-16).
func Review(cfg *config.Config, name string, workspaces []string, effective EffectiveMounts, lookPath func(string) (string, error), opts ReviewOptions) (*ReviewResult, error) {
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		return nil, err
	}
	loaded, err := Load(cfg, &ts.AcceptanceStore, name, workspaces, lookPath)
	if err != nil {
		return nil, err
	}
	bom, err := ComputeBoM(loaded, effective, lookPath)
	if err != nil {
		return nil, err
	}
	if !bom.Tier1() {
		return &ReviewResult{Accepted: true}, nil
	}

	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	if err := gate(opts.In, out, opts.TTY, opts.Yes, name, bom, opts.Verbose); err != nil {
		return nil, err
	}

	var result ReviewResult
	_, err = mutateEnvironmentTrustStoreLocked(func(s *environmentTrustStore) error {
		// Reload and recompute FRESH, under this same lock, immediately
		// before the fingerprint that gets persisted is derived — see the
		// package doc comment's "TOCTOU" section. A mutation that landed
		// after the render above (a symlink swapped in, a command changed)
		// is caught HERE, by the exact same refusals a first Load would
		// hit, rather than silently accepted under a fingerprint of a
		// surface nobody actually reviewed.
		reloaded, err := Load(cfg, &s.AcceptanceStore, name, workspaces, lookPath)
		if err != nil {
			return err
		}
		freshBoM, err := ComputeBoM(reloaded, effective, lookPath)
		if err != nil {
			return err
		}
		fp, err := Fingerprint(freshBoM)
		if err != nil {
			return err
		}
		s.Put(Subject(reloaded.Root), hosttrust.Record{Fingerprint: fp})
		result = ReviewResult{Accepted: true, Fingerprint: fp}
		return nil
	})
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "pix: recorded acceptance for environment %q (fingerprint %s).\n", name, shortFingerprint(result.Fingerprint))
	return &result, nil
}

// shortFingerprint is the short-hash form the success line names —
// git-short-hash-shaped, never the full 64 hex chars.
func shortFingerprint(fp string) string {
	const n = 12
	if len(fp) <= n {
		return fp
	}
	return fp[:n]
}
