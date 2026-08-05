// Package corpus is the W0 U00b golden corpus + retirement-manifest harness
// for pix's CLI surface: a compact, deterministic, per-verb/flag-sharded set
// of characterization tests (help text, bad invocation, safe read-only output
// shape, JSON key contracts, stdout/stderr, exit codes) run against the REAL
// compiled `pix` binary, plus an append-only retirement manifest that records
// approved verb/flag deletions.
//
// It exists to guard later deletion and command migration: a shard proves
// what a verb does TODAY, and it stays green until either the behavior stops
// changing (normal development) or a maintainer explicitly retires the verb
// (moving its case into retirement.jsonl, never editing history in place).
//
// Everything this package runs is help/bad-flag/local-read-only: no verb case
// may reach a destructive or network operation (see
// TestShards_ForbidDangerousArgvPrefixes and the isolated-HOME contract in
// RunCase). This package touches nothing outside a test's own t.TempDir().
//
// This package is TEST-ONLY SUPPORT, not a runtime dependency: every .go file
// in this directory is a _test.go file, nothing outside `go test` ever
// imports it, and it is deliberately absent from docs/design/architecture.md's
// layer map and scripts/arch-metrics/budgets.json — there is no production
// LOC here to place or budget. `go test ./cmd/pix/corpus` (and the full
// golden-corpus run in CI's `metrics` job) is the only way this code runs.
package corpus

// Case is one golden invocation of the pix CLI: an argv, the isolated
// environment/working directory it runs in, and the contract its output must
// satisfy.
type Case struct {
	// Name is unique within its Shard (used as the subtest name).
	Name string `json:"name"`
	// Args is the full argv passed to the pix binary (verb first), e.g.
	// []string{"config", "show"}. Never empty.
	Args []string `json:"args"`
	// ExitCode is the exact process exit code this invocation must produce.
	ExitCode int `json:"exitCode"`
	// Stream selects which stream Contains/JSONKeys are checked against:
	// "stdout" (default, zero value) or "stderr".
	Stream string `json:"stream,omitempty"`
	// Contains lists substrings that must all appear in the selected stream.
	Contains []string `json:"contains,omitempty"`
	// JSONKeys, when non-empty, asserts the selected stream parses as a JSON
	// array of objects, each with EXACTLY this key set (order-independent).
	// Values are intentionally NOT checked — a JSON key contract guards
	// structural shape (a field renamed/removed breaks a downstream script),
	// not the data behind it, which is expected to drift.
	JSONKeys []string `json:"jsonKeys,omitempty"`
}

// Shard is every golden case for one verb. One file per verb keeps the corpus
// compact (a reviewer diffs exactly the verb that changed) and lets the
// deletion-guard test enumerate the CLI's live surface by directory listing.
type Shard struct {
	Verb  string `json:"verb"`
	Cases []Case `json:"cases"`
}

// Result is what actually came back from running a Case.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// RetirementEntry is one approved, append-only record that a verb or a single
// flag on a verb was deliberately removed from the CLI. Granularity is
// "verb" (Flag must be empty) or "flag" (Flag must be set, e.g. "--profile").
//
// Entries are identified and ordered by Seq, a strictly increasing integer
// starting at 1 with no gaps — the manifest's only valid mutation is
// appending the next Seq at the end of the file (see CheckAppendOnly).
type RetirementEntry struct {
	Seq         int    `json:"seq"`
	Granularity string `json:"granularity"` // "verb" | "flag"
	Verb        string `json:"verb"`
	Flag        string `json:"flag,omitempty"`
	Status      string `json:"status"` // manifest holds "approved" entries only
	ApprovedBy  string `json:"approvedBy"`
	ApprovedAt  string `json:"approvedAt"`
	Reason      string `json:"reason"`
	Replacement string `json:"replacement,omitempty"`
}
