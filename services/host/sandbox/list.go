package sandbox

import (
	"encoding/json"
	"fmt"
	"strings"
)

// State is the sandbox liveness state: a parsed listing row resolves to
// Unknown/Running/Stopped for its own lifecycle column, and a whole-probe
// caller (e.g. workflow/launch, see its SbxState alias) additionally uses
// Absent for "probe ran clean, name not present". Unknown is returned
// whenever the raw value is not among the documented aliases below — it is
// NEVER guessed into Running or Stopped, the fail-closed posture this
// package and workflow/launch's PlanSandboxLaunch both hold for sandbox
// liveness (an unrecognized/unreadable state must not be mistaken for a
// known-safe one).
type State int

const (
	StateUnknown State = iota
	StateRunning
	StateStopped
	// StateAbsent is the fourth state a WHOLE-PROBE result (not a listing
	// row) can resolve to: the probe positively confirmed name is NOT
	// present, distinct from StateUnknown's "the probe itself could not be
	// trusted". A ParseList row never carries this value on its own — it
	// exists for callers (e.g. workflow/launch's sandbox-liveness probe)
	// that fold "ran clean, name missing from the output" into the same
	// state type as running/stopped/unknown rather than inventing a sibling
	// enum for what is the same fail-closed tri/four-state posture.
	StateAbsent
)

// String renders State for messages/tests.
func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// runningAliases / stoppedAliases are the documented VALUE aliases a state
// field may carry. This is a value-alias table (what the field says),
// distinct from the KEY-alias tables below (what the field is called) —
// see Entry.IdentityVerified's doc for why the two are tracked separately.
var runningAliases = map[string]bool{
	"running": true, "up": true, "started": true,
}

var stoppedAliases = map[string]bool{
	"stopped": true, "exited": true, "created": true,
	"paused": true, "restarting": true, "dead": true, "down": true,
}

func stateFromValue(raw string) State {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case runningAliases[v]:
		return StateRunning
	case stoppedAliases[v]:
		return StateStopped
	default:
		return StateUnknown
	}
}

// nameKeys / stateKeys / idKeys are the documented KEY aliases this parser
// recognizes for each field, canonical spelling FIRST. Using anything past
// index 0 is what flips a row's IdentityVerified to false — see doc.go's
// "Schema posture" section for why: without a pinned sbx schema, this is the
// best evidence available that the row means what this package assumes it
// means.
var nameKeys = []string{"name", "Name", "container_name", "Sandbox", "sandbox"}
var stateKeys = []string{"state", "State", "status", "Status"}
var idKeys = []string{"instance_id", "InstanceID", "instanceId", "id", "ID"}

// wrapperKeys are the documented alias container keys accepted when the top
// level is an object rather than a bare array. A bare array is the
// canonical/documented primary shape; using a wrapper key always marks the
// overall parse unverified (ParseResult.SchemaVerified), even if every row
// inside it is otherwise pristine.
var wrapperKeys = []string{"sandboxes", "items", "boxes"}

// Entry is one parsed listing row.
type Entry struct {
	Name  string
	State State
	// InstanceID is optional (nil when the row carried none of idKeys, or an
	// empty string under one). Once populated for a given Name, treat it as
	// IMMUTABLE identity: this package never rewrites or re-derives it, and a
	// caller correlating it against another store's own identity record
	// (e.g. a lease.Record.InstanceID) must never accept a later value that
	// changes for the same Name — that would mean the name was reused for a
	// different underlying instance, not that the identity was updated.
	InstanceID *string
	// IdentityVerified is false whenever ANY field on this row required a
	// KEY alias rather than the canonical key, or the row carried a key
	// outside the full documented set (nameKeys ∪ stateKeys ∪ idKeys). It is
	// UNRELATED to whether the state VALUE was recognized — an unrecognized
	// state value alone (State == StateUnknown) does not, by itself, make a
	// row unverified, because the key it came from can still be exactly the
	// one this package expects. A false here means: parsed leniently, but do
	// not trust this row's identity for a destructive decision.
	IdentityVerified bool
}

// ParseResult is ParseList's outcome.
type ParseResult struct {
	Entries []Entry
	// SchemaVerified is true only when the top level was a bare JSON array
	// (the documented canonical shape) AND every entry is itself
	// IdentityVerified. Entries are always returned regardless — this flag
	// is a convenience "trust the whole listing" fold of the per-entry
	// flags, not a gate on parsing.
	SchemaVerified bool
}

// ParseList parses raw bytes shaped like an `sbx`-style sandbox listing. It
// never panics and never silently drops a row it cannot name: a row missing
// every documented name-key alias fails the WHOLE parse (fail-closed —
// silently dropping an unparseable row could hide a live sandbox from a
// caller deciding whether one already exists). A row it CAN name but had to
// lean on aliases for is returned with IdentityVerified=false rather than
// rejected.
func ParseList(data []byte) (ParseResult, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return ParseResult{}, fmt.Errorf("sandbox: invalid JSON: %w", err)
	}
	rows, wrapperVerified, err := unwrapRows(raw)
	if err != nil {
		return ParseResult{}, err
	}
	result := ParseResult{SchemaVerified: wrapperVerified}
	for i, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return ParseResult{}, fmt.Errorf("sandbox: row %d is not a JSON object", i)
		}
		entry, err := parseRow(m)
		if err != nil {
			return ParseResult{}, fmt.Errorf("sandbox: row %d: %w", i, err)
		}
		if !entry.IdentityVerified {
			result.SchemaVerified = false
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

// unwrapRows finds the row array: a bare array is canonical (verified=true);
// an object with one of wrapperKeys is an accepted alias shape
// (verified=false). Anything else is a parse error.
func unwrapRows(raw any) (rows []any, verified bool, err error) {
	switch v := raw.(type) {
	case []any:
		return v, true, nil
	case map[string]any:
		for _, k := range wrapperKeys {
			arr, ok := v[k]
			if !ok {
				continue
			}
			list, ok := arr.([]any)
			if !ok {
				return nil, false, fmt.Errorf("sandbox: %q is not a JSON array", k)
			}
			return list, false, nil
		}
		return nil, false, fmt.Errorf("sandbox: object has none of the documented list keys %v", wrapperKeys)
	default:
		return nil, false, fmt.Errorf("sandbox: top level is neither a JSON array nor object")
	}
}

// firstKey returns the first key from keys present in m, the raw value, and
// whether it was the CANONICAL (index-0) key.
func firstKey(m map[string]any, keys []string) (key string, val any, canonical bool) {
	for i, k := range keys {
		if v, ok := m[k]; ok {
			return k, v, i == 0
		}
	}
	return "", nil, false
}

func parseRow(m map[string]any) (Entry, error) {
	verified := true

	nameKey, nameVal, nameCanonical := firstKey(m, nameKeys)
	if nameKey == "" {
		return Entry{}, fmt.Errorf("no name field among the documented aliases %v", nameKeys)
	}
	name, ok := nameVal.(string)
	if !ok || name == "" {
		return Entry{}, fmt.Errorf("field %q is not a non-empty string", nameKey)
	}
	if !nameCanonical {
		verified = false
	}

	state := StateUnknown
	if stateKey, stateVal, stateCanonical := firstKey(m, stateKeys); stateKey != "" {
		if s, ok := stateVal.(string); ok {
			state = stateFromValue(s)
		}
		if !stateCanonical {
			verified = false
		}
	}

	var instanceID *string
	if idKey, idVal, idCanonical := firstKey(m, idKeys); idKey != "" {
		if s, ok := idVal.(string); ok && s != "" {
			instanceID = &s
		}
		if !idCanonical {
			verified = false
		}
	}

	if hasUndocumentedKeys(m) {
		verified = false
	}

	return Entry{Name: name, State: state, InstanceID: instanceID, IdentityVerified: verified}, nil
}

// hasUndocumentedKeys reports whether m carries any key outside the full
// documented set (every name/state/id key alias combined). A row from a
// genuinely different (unknown-to-us) schema will typically carry at least
// one such key, which is exactly the signal IdentityVerified exists to
// surface even when the keys it DOES recognize happened to line up.
func hasUndocumentedKeys(m map[string]any) bool {
	documented := map[string]bool{}
	for _, k := range nameKeys {
		documented[k] = true
	}
	for _, k := range stateKeys {
		documented[k] = true
	}
	for _, k := range idKeys {
		documented[k] = true
	}
	for k := range m {
		if !documented[k] {
			return true
		}
	}
	return false
}

// FindByName returns the entry named name from entries, or nil if absent.
// "Absent" here means only "not in THIS listing" — it carries no claim about
// whether the listing itself was trustworthy (see ParseResult.SchemaVerified)
// or complete.
func FindByName(entries []Entry, name string) *Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}
