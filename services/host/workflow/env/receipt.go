// receipt.go — the itemized form of the SAME canonical document
// Fingerprint hashes, so that a re-gated environment can be reviewed as a
// CHANGE rather than as a second full audit dump.
//
// The fingerprint answers one bit: did this environment's host-exec surface
// change since you accepted it. It cannot answer WHAT changed, so a
// one-line kit bump and a newly added host command are, on screen, the same
// event: read the whole bill again. That is the trust-fatigue failure mode
// the migration plan's own risk table names ("kit fingerprint churn ->
// trust fatigue -> people run --yes reflexively"), and a review nobody
// reads is not a gate.
//
// A Receipt is the per-item index of the accepted surface: one entry per
// reviewable fact, carrying the SECTION it belongs to, the legible KEY that
// identifies it, and a DIGEST of everything else about it. It is recorded
// alongside the fingerprint at acceptance and diffed against the current
// environment at the next gate.
//
// Two properties are load-bearing, and receipt_test.go pins both:
//
//  1. The receipt is derived from canonicalDoc — the one value Fingerprint
//     hashes — never from a parallel walk of BillOfMaterials. A section
//     that joins the fingerprint therefore joins the receipt in the same
//     edit, and TestReceiptCoversEveryFingerprintedSection fails the build
//     if a new fpDoc field is added without one.
//  2. DIFFERENT FINGERPRINTS IMPLY A NON-EMPTY DIFF. A change screen that
//     can print "nothing changed" while the gate is refusing entry would be
//     worse than no change screen at all: it would train the reviewer that
//     the re-gate is noise. TestFingerprintChangeAlwaysDiffs proves it
//     across a mutation of every section.
//
// What a receipt deliberately does NOT store is any item's detail beyond its
// key: only sha256 digests. An accepted-state file that accumulated every
// argv, mount path, and credential destination of every environment a person
// ever trusted would be a second copy of that surface with no reader and its
// own disclosure risk. The change screen prints what a fact is NOW from the
// live in-memory bill of materials, and names a REMOVED fact by its key
// alone, which is all a reviewer needs to decide about a removal.
package env

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"pix/host/envinfo"
	"pix/host/hosttrust"
)

// ReceiptEntry is one reviewable fact of an accepted bill of materials.
// Section is the fpDoc field it came from (the same grouping the consent
// screen renders in), Key is the legible identifier a human matches on, and
// Digest covers the ENTIRE item including the key — so a fact whose name is
// unchanged but whose argv, digest, or destination moved reads as `changed`,
// not as unchanged.
type ReceiptEntry struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Digest  string `json:"digest"`
}

// ChangeKind is what happened to one (section, key) between two receipts.
type ChangeKind string

const (
	// ChangeAdded is a fact present now and absent at acceptance.
	ChangeAdded ChangeKind = "added"
	// ChangeRemoved is a fact accepted then and absent now. It is named by
	// key only: its detail was never stored.
	ChangeRemoved ChangeKind = "removed"
	// ChangeChanged is a fact whose key survived and whose content did not.
	ChangeChanged ChangeKind = "changed"
)

// ReceiptChange is one line of a change diff.
type ReceiptChange struct {
	Kind    ChangeKind
	Section string
	Key     string
}

// Receipt itemizes b exactly as Fingerprint hashes it. The returned slice is
// in canonical order (section order as declared below, then the canonical
// within-section order Fingerprint already imposes), so two receipts of the
// same bill of materials are byte-identical.
func Receipt(b BillOfMaterials) ([]ReceiptEntry, error) {
	doc := canonicalDoc(b)
	var out []ReceiptEntry
	var firstErr error
	add := func(section, key string, item any) {
		d, err := itemDigest(item)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("itemizing environment %s %q: %v", section, key, err)
			}
			return
		}
		out = append(out, ReceiptEntry{Section: section, Key: key, Digest: d})
	}

	// The schema version is itself a reviewable fact: a bump re-gates every
	// accepted environment (Fingerprint's own doc comment says so), and the
	// change screen must be able to say that is why rather than reporting a
	// surface that did not move.
	add("schema", "fingerprint version", doc.V)

	sect(add, "host command", doc.HostCommands, func(v HostCommand) string { return v.Name })
	sect(add, "host service", doc.HostServices, func(v HostServiceItem) string { return v.Name })
	sect(add, "setup hook", doc.SetupHooks, func(v SetupHookFact) string { return v.ID })
	sect(add, "credential", doc.CredentialTargets, func(v CredentialTarget) string {
		return v.Source + " -> " + v.Destination
	})
	sect(add, "mount", []WorkspaceMount(doc.EffectiveMounts), func(v WorkspaceMount) string { return v.Path })
	sect(add, "secret", doc.Secrets, func(v SecretFact) string { return v.Name })
	sect(add, "registry", doc.Registries, func(v RegistryFact) string { return v.Host })
	sect(add, "binding", doc.Bindings, func(v BindingFact) string { return v.Service })
	sect(add, "mcp server", doc.MCPServers, func(v MCPServerFact) string { return v.Name })
	sect(add, "port", doc.Ports, func(v PortFact) string { return fmt.Sprintf("%d", v.Sandbox) })
	// Kits and Interpolations are the two sections canonicalDoc leaves in
	// AUTHORED order, because for kits that order is semantic (a later mixin
	// overlays an earlier one). Their digests therefore cover the position
	// too: a pure reorder re-gates, so a pure reorder must also produce a
	// non-empty diff, or the change screen would contradict the gate.
	ordered(add, "kit", doc.Kits, func(v KitFact) string { return v.Raw })
	sect(add, "host mcp", doc.HostMCP, func(v HostMCPFact) string { return v.Name })
	sect(add, "inference", doc.Inference, func(v InferenceFact) string { return v.Name })
	ordered(add, "interpolation", doc.Interpolations, func(v envinfo.Interpolation) string {
		return "${" + v.Var + "} -> " + v.KeyPath
	})

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// sect itemizes one fpDoc section. It exists so that adding a section is one
// line that cannot forget the digest, and so receiptSections (the guard
// test's input) and this list are the same list.
func sect[T any](add func(section, key string, item any), section string, items []T, key func(T) string) {
	for _, it := range items {
		add(section, key(it), it)
	}
}

// ordered is sect for a section whose POSITION is part of the fingerprint:
// the digest covers {position, item}, so moving an item without editing it
// still reads as a change on that key.
func ordered[T any](add func(section, key string, item any), section string, items []T, key func(T) string) {
	for i, it := range items {
		add(section, key(it), struct {
			I int `json:"i"`
			V T   `json:"v"`
		}{i, it})
	}
}

// itemDigest hashes one item's canonical JSON through the SAME encoder the
// fingerprint uses, so an item that is identical for fingerprint purposes is
// identical here and an item that is not, is not.
func itemDigest(item any) (string, error) {
	canonical, err := hosttrust.Canonicalize(item)
	if err != nil {
		return "", err
	}
	fp, err := hosttrust.Fingerprint(canonical)
	if err != nil {
		return "", err
	}
	return fp, nil
}

// DiffReceipts reports what moved between the receipt recorded at
// acceptance (prev) and the receipt of the environment as it is now (cur).
// The result is ordered by section (in Receipt's declared order) then key,
// so a diff renders deterministically.
//
// Duplicate keys within a section are compared as a MULTISET of digests, not
// pairwise by position: two host commands legitimately sharing a name differ
// only in their digests, and a diff that matched them positionally would
// report a reorder as a change (and, worse, could report a real swap as
// nothing).
func DiffReceipts(prev, cur []ReceiptEntry) []ReceiptChange {
	type group struct {
		section, key string
	}
	order := map[string]int{}
	index := func(entries []ReceiptEntry) map[group][]string {
		m := map[group][]string{}
		for i, e := range entries {
			g := group{e.Section, e.Key}
			m[g] = append(m[g], e.Digest)
			if _, seen := order[e.Section]; !seen {
				order[e.Section] = i
			}
		}
		for _, digests := range m {
			sort.Strings(digests)
		}
		return m
	}
	// cur is indexed first so section ordering follows the CURRENT
	// environment; a section that only prev had lands after it, which is the
	// right reading order for "here is what you have now, and here is what
	// went away".
	curIdx, prevIdx := index(cur), index(prev)

	var out []ReceiptChange
	for g, curDigests := range curIdx {
		prevDigests, had := prevIdx[g]
		switch {
		case !had:
			out = append(out, ReceiptChange{Kind: ChangeAdded, Section: g.section, Key: g.key})
		case !sameDigests(prevDigests, curDigests):
			out = append(out, ReceiptChange{Kind: ChangeChanged, Section: g.section, Key: g.key})
		}
	}
	for g := range prevIdx {
		if _, still := curIdx[g]; !still {
			out = append(out, ReceiptChange{Kind: ChangeRemoved, Section: g.section, Key: g.key})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if oi, oj := order[out[i].Section], order[out[j].Section]; oi != oj {
			return oi < oj
		}
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func sameDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// UnchangedCount is how many of cur's facts the diff did NOT name. The
// change screen prints it so a reviewer knows the size of the surface the
// three named lines sit in — "3 changed of 4" and "3 changed of 60" are
// different decisions.
func UnchangedCount(cur []ReceiptEntry, changes []ReceiptChange) int {
	touched := map[string]bool{}
	for _, c := range changes {
		if c.Kind == ChangeRemoved {
			continue
		}
		touched[c.Section+"\x00"+c.Key] = true
	}
	n := 0
	for _, e := range cur {
		if !touched[e.Section+"\x00"+e.Key] {
			n++
		}
	}
	return n
}

// receiptDigestOfEntries is the fingerprint of a receipt itself, used by the
// guard test to assert receipt equality implies canonical-document equality.
func receiptDigestOfEntries(entries []ReceiptEntry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", e.Section, e.Key, e.Digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}
