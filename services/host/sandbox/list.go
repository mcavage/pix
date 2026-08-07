package sandbox

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// v38WrapperKey is the object-wrapper key sbx v0.38 emits for `sbx ls
// --json`: the CANONICAL top level for the v0.38 row profile is
// `{"sandboxes": [...]}`, verified to the same degree a bare array is for
// the legacy profile — NOT an unverified alias, because this is the exact
// shape captured from a real v0.38 install (see doc.go's "Schema posture").
const v38WrapperKey = "sandboxes"

// legacyAliasWrapperKeys are the documented alias container keys accepted
// when the top level is an object using the LEGACY row profile rather than
// v38WrapperKey. A bare array is the legacy profile's canonical primary
// shape; wrapping it in one of these always marks the overall parse
// unverified (ParseResult.SchemaVerified), even if every row inside it is
// otherwise pristine.
var legacyAliasWrapperKeys = []string{"items", "boxes"}

// v38RowKeys are the exact, complete row keys sbx v0.38 emits per sandbox:
// name, id (a UUID), agent (string), status (a recognized value), workspaces
// (array), workspace_missing (bool). Unlike the legacy profile's nameKeys/
// stateKeys/idKeys, this profile has NO key aliases — a v38-wrapped row using
// a legacy alias (e.g. "instance_id" instead of "id") is a key outside the
// selected profile, which this package treats the same as any other
// undocumented key: never silently accepted as canonical.
var v38RowKeys = []string{"name", "id", "agent", "status", "workspaces", "workspace_missing"}

// v38UUIDPattern is the shape check for the v0.38 row's "id" field: canonical
// 8-4-4-4-12 hyphenated hex, the documented UUID form. It does not pin a
// version/variant nibble — sbx does not document one — only the dash/hex
// shape that distinguishes a real identity from an arbitrary string.
var v38UUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	// SchemaVerified is true only when the top level was ONE of the two
	// documented canonical shapes — a bare JSON array (legacy) or
	// `{"sandboxes": [...]}` (v0.38) — AND every entry is itself
	// IdentityVerified. Entries are always returned regardless — this flag
	// is a convenience "trust the whole listing" fold of the per-entry
	// flags, not a gate on parsing.
	SchemaVerified bool
}

// ParseList parses raw bytes shaped like an `sbx`-style sandbox listing,
// against one of two documented, mutually exclusive row profiles selected by
// the top-level shape alone (see unwrapRows): a bare array uses the legacy
// profile (nameKeys/stateKeys/idKeys, alias-tolerant); `{"sandboxes": [...]}`
// uses the v0.38 profile (v38RowKeys, alias-free — see parseRowV38). It
// never panics and never silently drops a row it cannot name: a row missing
// every documented name-key alias, or (under the v0.38 profile) missing or
// mistyping id/status/agent/workspaces/workspace_missing, fails the WHOLE
// parse (fail-closed — silently dropping an unparseable row could hide a
// live sandbox from a caller deciding whether one already exists). A row it
// CAN name but had to lean on a legacy alias for, or that carried an
// undocumented extra key, or (v0.38) an unrecognized status value, is
// returned with IdentityVerified=false rather than rejected.
func ParseList(data []byte) (ParseResult, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return ParseResult{}, fmt.Errorf("sandbox: invalid JSON: %w", err)
	}
	rows, wrapperVerified, prof, err := unwrapRows(raw)
	if err != nil {
		return ParseResult{}, err
	}
	result := ParseResult{SchemaVerified: wrapperVerified}
	for i, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return ParseResult{}, fmt.Errorf("sandbox: row %d is not a JSON object", i)
		}
		var entry Entry
		var perr error
		if prof == profileV38 {
			entry, perr = parseRowV38(m)
		} else {
			entry, perr = parseRow(m)
		}
		if perr != nil {
			return ParseResult{}, fmt.Errorf("sandbox: row %d: %w", i, perr)
		}
		if !entry.IdentityVerified {
			result.SchemaVerified = false
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

// profile identifies which canonical row shape unwrapRows selected, from the
// TOP LEVEL alone: every row in one listing is checked against exactly one
// profile, never a per-row mix. profileLegacy also covers the object-wrapped
// legacyAliasWrapperKeys shapes, unchanged from before v38 existed.
type profile int

const (
	profileLegacy profile = iota
	profileV38
)

// unwrapRows finds the row array and which profile its rows must be checked
// against: a bare array is the legacy canonical shape (verified=true,
// profileLegacy); an object keyed v38WrapperKey is the v0.38 canonical shape
// (verified=true, profileV38); an object keyed one of legacyAliasWrapperKeys
// is an accepted alias of the legacy shape (verified=false, profileLegacy).
// Anything else is a parse error.
func unwrapRows(raw any) (rows []any, verified bool, prof profile, err error) {
	switch v := raw.(type) {
	case []any:
		return v, true, profileLegacy, nil
	case map[string]any:
		if arr, ok := v[v38WrapperKey]; ok {
			list, ok := arr.([]any)
			if !ok {
				return nil, false, profileLegacy, fmt.Errorf("sandbox: %q is not a JSON array", v38WrapperKey)
			}
			return list, true, profileV38, nil
		}
		for _, k := range legacyAliasWrapperKeys {
			arr, ok := v[k]
			if !ok {
				continue
			}
			list, ok := arr.([]any)
			if !ok {
				return nil, false, profileLegacy, fmt.Errorf("sandbox: %q is not a JSON array", k)
			}
			return list, false, profileLegacy, nil
		}
		documented := append([]string{v38WrapperKey}, legacyAliasWrapperKeys...)
		return nil, false, profileLegacy, fmt.Errorf("sandbox: object has none of the documented list keys %v", documented)
	default:
		return nil, false, profileLegacy, fmt.Errorf("sandbox: top level is neither a JSON array nor object")
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

	if hasUndocumentedKeys(m, nameKeys, stateKeys, idKeys) {
		verified = false
	}

	return Entry{Name: name, State: state, InstanceID: instanceID, IdentityVerified: verified}, nil
}

// parseRowV38 parses one row of the v0.38 canonical profile: name, id, agent,
// status, workspaces, workspace_missing, ALL required with the exact
// canonical key (no aliases — see v38RowKeys' doc) and the exact documented
// type. Unlike the legacy profile, where a missing/wrong-typed id or state is
// tolerated (id is optional; an unreadable state value degrades to
// StateUnknown), the v0.38 evidence pins exactly what a genuine row contains,
// so a missing field, a wrong type, or an id that isn't shaped like a UUID is
// NOT something this package can parse leniently and still vouch for — it
// fails the whole listing, the same fail-closed treatment the legacy profile
// already gives an unresolvable name. An undocumented EXTRA key, or a
// well-typed but unrecognized status value, downgrades IdentityVerified
// instead of erroring: the row is still legibly a v0.38 row, just one this
// package cannot fully vouch for.
func parseRowV38(m map[string]any) (Entry, error) {
	name, ok := m["name"].(string)
	if !ok || name == "" {
		if _, present := m["name"]; !present {
			return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "name")
		}
		return Entry{}, fmt.Errorf("field %q is not a non-empty string", "name")
	}

	idVal, present := m["id"]
	if !present {
		return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "id")
	}
	id, ok := idVal.(string)
	if !ok || !v38UUIDPattern.MatchString(id) {
		return Entry{}, fmt.Errorf("field %q is not a UUID", "id")
	}

	statusVal, present := m["status"]
	if !present {
		return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "status")
	}
	status, ok := statusVal.(string)
	if !ok || status == "" {
		return Entry{}, fmt.Errorf("field %q is not a non-empty string", "status")
	}

	agentVal, present := m["agent"]
	if !present {
		return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "agent")
	}
	if _, ok := agentVal.(string); !ok {
		return Entry{}, fmt.Errorf("field %q is not a string", "agent")
	}

	workspacesVal, present := m["workspaces"]
	if !present {
		return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "workspaces")
	}
	if _, ok := workspacesVal.([]any); !ok {
		return Entry{}, fmt.Errorf("field %q is not an array", "workspaces")
	}

	wmVal, present := m["workspace_missing"]
	if !present {
		return Entry{}, fmt.Errorf("v0.38 row missing required field %q", "workspace_missing")
	}
	if _, ok := wmVal.(bool); !ok {
		return Entry{}, fmt.Errorf("field %q is not a boolean", "workspace_missing")
	}

	verified := true
	state := stateFromValue(status)
	if state == StateUnknown {
		// Well-typed but unrecognized: the row parses, but the v0.38 evidence
		// this profile is pinned to says a genuine row's status is always one
		// of the documented aliases, so an unrecognized value is itself a
		// reason not to vouch for the row's identity.
		verified = false
	}
	if hasUndocumentedKeys(m, v38RowKeys) {
		verified = false
	}

	instanceID := id
	return Entry{Name: name, State: state, InstanceID: &instanceID, IdentityVerified: verified}, nil
}

// hasUndocumentedKeys reports whether m carries any key outside the given
// documented sets. A row from a genuinely different (unknown-to-us) schema
// will typically carry at least one such key, which is exactly the signal
// IdentityVerified exists to surface even when the keys it DOES recognize
// happened to line up.
func hasUndocumentedKeys(m map[string]any, keySets ...[]string) bool {
	documented := map[string]bool{}
	for _, keys := range keySets {
		for _, k := range keys {
			documented[k] = true
		}
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
