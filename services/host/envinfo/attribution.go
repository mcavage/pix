// attribution.go — E2.2's creation fingerprint + drift attribution map.
//
// Two functions, one each way:
//
//   - ComputeFingerprint fingerprints every EFFECTIVE (post-composition)
//     create-time semantic facet, consuming E2.1's RenderEffective inputs
//     directly (the same RuntimeFacts value, and the SAME private
//     effective* composition helpers render.go already uses — never a
//     second, independently-shaped composition). It does NOT consume
//     BuildTree's pre-composition Tree for its OWN values: the fingerprint
//     is over what actually gets created, not what was merely authored.
//   - Attribute maps a CHANGED composed facet back to E1.2's
//     PRE-composition Tree, so a drift report names the stable authored
//     key path a reviewer already approved — never the composed shape
//     alone, and never a hash.
//
// # Comments/formatting vs. canonical values
//
// ComputeFingerprint only ever reads already-Parsed/Merged Go values
// (Document fields, map values) — never re-reads a YAML file's bytes. A
// comment or a re-indented authored file changes NOTHING here because
// nothing here ever sees the file's bytes at all; only Parse (upstream of
// this package's own contract) could introduce that kind of noise, and
// Parse already discards comments and whitespace as an ordinary side
// effect of decoding into typed Go values.
//
// # Interpolation: authored expression + keyed HMAC, never the raw value
//
// A facet value that still carries an authored `${VAR}` (or
// `${VAR:-default}`) expression is fingerprinted as the expression text
// PLUS an opaque digest an InterpolationResolver returns — never the
// resolved value itself, and never an unkeyed hash of it (that would be
// offline-guessable for a low-entropy secret: see
// hosttrust/hmackey_test.go's guessing sentinel). envinfo cannot compute
// that digest itself: hosttrust owns the one launcher-keyed HMAC key
// record, and envinfo (an L1 capability) may not import hosttrust (an L1
// sibling) — see doc.go. So the digest arrives already-signed, via a
// caller-supplied InterpolationResolver; this package never sees, holds,
// or could accidentally log a raw resolved value or the key itself.
//
// # Reset invalidation, not a per-field flood
//
// When the resolver reports the launcher HMAC key is gone (the exact
// aftermath of `pix reset` moving the whole config dir aside),
// ComputeFingerprint fails closed with ErrHMACKeyMissing rather than
// silently falling back to an unkeyed comparison. The caller turns that
// ONE error into ONE attributed drift record (ResetInvalidatedDrift),
// never a separate refusal per authored ${VAR} reference or per changed
// key — see ResetInvalidatedDrift's own doc comment for why that
// distinction matters to recreatelog's bounded cap.
package envinfo

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Fingerprint is a composed-facet-keyed map of canonical fingerprint
// inputs — envinfo's own analog of sandbox.Fingerprint, over the RICHER
// composed-facet key space this package owns (see sandbox.FromFacetMap,
// which adapts one of these into a sandbox.Fingerprint so the existing
// Diff/Equal comparison engine works unchanged over it).
type Fingerprint map[string]string

// InterpolationResolver resolves and SIGNS one authored ${VAR} (or
// ${VAR:-default}) reference for fingerprinting. It must return an opaque,
// launcher-keyed digest ONLY — never the raw resolved value, and never an
// unkeyed hash of it. Returning ErrHMACKeyMissing (or an error satisfying
// errors.Is against it) tells ComputeFingerprint the launcher's HMAC key
// record is gone; every other error is propagated as an ordinary failure.
// A nil resolver is valid: a facet containing an authored expression is
// still fingerprinted on that expression text alone (comparison then just
// cannot detect a resolved-value-only change until a resolver is
// supplied — e.g. a --effective PREVIEW with no live host to resolve
// against).
type InterpolationResolver func(varName string, def *string) (digest string, err error)

// ErrHMACKeyMissing is the sentinel an InterpolationResolver returns when
// the launcher-keyed HMAC key this package needs to fingerprint an
// interpolated facet no longer exists — see hosttrust.
// ErrCreationHMACKeyMissing, which a caller composing a resolver maps to
// this sentinel (envinfo may not import hosttrust to compare against its
// error directly; see doc.go's sibling-isolation rule).
var ErrHMACKeyMissing = errors.New(
	"envinfo: launcher HMAC key missing; acceptance invalidated by reset")

// ComputeFingerprint fingerprints every effective create-time semantic
// facet RenderEffective would compose from facts, keyed by its COMPOSED
// (post-composition) address. It is deterministic for a fixed facts value
// and a resolver that answers deterministically.
func ComputeFingerprint(facts RuntimeFacts, resolve InterpolationResolver) (Fingerprint, error) {
	doc := facts.Document
	if doc == nil {
		return nil, ErrNoDocument
	}
	fp := Fingerprint{}
	set := func(key, raw string) error {
		v, err := canonicalizeFacetValue(raw, resolve)
		if err != nil {
			return err
		}
		fp[key] = v
		return nil
	}

	if err := set("schemaVersion", effectiveSchemaVersion(doc)); err != nil {
		return nil, err
	}
	if err := set("agent", doc.Agent); err != nil {
		return nil, err
	}
	if err := set("name", effectiveName(facts)); err != nil {
		return nil, err
	}

	for i, k := range effectiveKits(facts) {
		if err := set(fmt.Sprintf("kits[%d]", i), k); err != nil {
			return nil, err
		}
	}

	for i, m := range effectiveWorkspaces(facts) {
		v := fmt.Sprintf("%s|readOnly=%v|clone=%v", m.Path, m.ReadOnly, m.Clone)
		if err := set(fmt.Sprintf("mounts[%d]", i), v); err != nil {
			return nil, err
		}
	}

	opts := effectiveSandboxOptions(facts)
	for _, k := range sortedKeys(opts) {
		if err := set("sandboxOptions."+k, opts[k]); err != nil {
			return nil, err
		}
	}

	env := effectiveEnv(facts)
	for _, k := range sortedKeys(env) {
		if err := set("env."+k, env[k]); err != nil {
			return nil, err
		}
	}

	secrets := effectiveSecrets(doc)
	for _, name := range sortedEffectiveSecretKeys(secrets) {
		s := secrets[name]
		if err := set("secrets."+name+".ref", s.Ref); err != nil {
			return nil, err
		}
		for i, c := range s.Command {
			if err := set(fmt.Sprintf("secrets.%s.command[%d]", name, i), c); err != nil {
				return nil, err
			}
		}
	}

	registries := effectiveRegistries(doc)
	for _, host := range sortedEffectiveRegistryKeys(registries) {
		r := registries[host]
		if err := set("registries."+host+".ref", r.Ref); err != nil {
			return nil, err
		}
		if err := set("registries."+host+".noVerify", strconv.FormatBool(r.NoVerify)); err != nil {
			return nil, err
		}
		for i, c := range r.Command {
			if err := set(fmt.Sprintf("registries.%s.command[%d]", host, i), c); err != nil {
				return nil, err
			}
		}
	}

	for _, svc := range sortedBindingMapKeys(doc.Bindings) {
		for _, d := range doc.Bindings[svc].APIKey.Domains {
			key := fmt.Sprintf("bindings.%s.apiKey.domains[%s]", svc, d)
			if err := set(key, d); err != nil {
				return nil, err
			}
		}
	}

	for _, srv := range effectiveMCPServers(facts) {
		base := fmt.Sprintf("mcp.servers[%s]", srv.Name)
		if err := set(base+".url", srv.URL); err != nil {
			return nil, err
		}
		if err := set(base+".command", srv.Command); err != nil {
			return nil, err
		}
		for i, a := range srv.Args {
			if err := set(fmt.Sprintf("%s.args[%d]", base, i), a); err != nil {
				return nil, err
			}
		}
	}

	for _, p := range doc.Ports {
		key := fmt.Sprintf("ports[%d].host", p.Sandbox)
		if err := set(key, strconv.Itoa(p.Host)); err != nil {
			return nil, err
		}
	}

	return fp, nil
}

// canonicalizeFacetValue is ComputeFingerprint's per-value step: an
// authored ${VAR}/${VAR:-default} expression is preserved VERBATIM (so a
// comment or reformatting elsewhere in the file — which never reaches this
// function at all, see the package doc comment — cannot possibly affect
// it) with one opaque, launcher-keyed digest appended per reference. A
// value with no interpolation is fingerprinted as-is.
func canonicalizeFacetValue(raw string, resolve InterpolationResolver) (string, error) {
	matches := interpolationRE.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 || resolve == nil {
		return raw, nil
	}
	var b strings.Builder
	b.WriteString(raw)
	for _, m := range matches {
		var def *string
		if m[2] != "" {
			d := m[3]
			def = &d
		}
		digest, err := resolve(m[1], def)
		if err != nil {
			return "", err
		}
		b.WriteString("|hmac:" + digest)
	}
	return b.String(), nil
}

// Drift is one attributed change between a stored and a current
// Fingerprint. Message is always human-readable and NEVER consists solely
// of a hash — either a stable pre-composition KeyPath (identity-bearing)
// or a collapsed "<list>[] (N entries changed)" count (identityless list).
type Drift struct {
	// ComposedKey is the post-composition address ComputeFingerprint used
	// ("env.FOO", "mcp.servers[github].url", "kits[2]") — or "kits[]"/
	// "mounts[]" for a collapsed identityless-list group, or "*" for
	// ResetInvalidatedDrift's single whole-environment record.
	ComposedKey string
	// KeyPath is the E1.2 PRE-composition stable key path this facet
	// traces back to, when one exists. Empty for a Pix-managed facet with
	// no authored source, and for a collapsed identityless-list group
	// (EntriesChanged > 0 instead).
	KeyPath string
	// Identity is true when this facet (or the composed key itself, for a
	// Pix-managed singleton with no authored source) has a stable identity
	// a reviewer can name directly — false for a collapsed identityless
	// list group.
	Identity bool
	// EntriesChanged is the number of changed index positions collapsed
	// into one identityless-list Drift; zero for an identity-bearing one.
	EntriesChanged int
	// Message is the always-non-hash-only human attribution line.
	Message string
}

// indexedListRE matches a whole composed key that is exactly one
// identityless, index-addressed list entry — "kits[3]", "mounts[0]" — the
// two composed lists this package ever collapses. Every other bracketed
// composed key (mcp.servers[name]..., ports[n]..., bindings...domains[d])
// carries stable identity and is matched against the pre-composition Tree
// instead; see identityKeyPaths.
var indexedListRE = regexp.MustCompile(`^(kits|mounts)\[\d+\]$`)

// Attribute maps every composed key that differs between stored and
// current back to pre's PRE-composition stable key paths. It assumes both
// fingerprints were computed successfully — a resolver that reported
// ErrHMACKeyMissing must never reach here; the caller synthesizes
// ResetInvalidatedDrift() instead (see this file's package doc comment).
func Attribute(pre *Tree, stored, current Fingerprint) []Drift {
	changed := diffFingerprintKeys(stored, current)
	if len(changed) == 0 {
		return nil
	}
	identity := identityKeyPaths(pre)
	listGroups := map[string][]string{}
	var drifts []Drift
	for _, key := range changed {
		if list, ok := indexedListName(key); ok {
			listGroups[list] = append(listGroups[list], key)
			continue
		}
		if kp, ok := findKeyPath(identity, key); ok {
			drifts = append(drifts, Drift{
				ComposedKey: key,
				KeyPath:     kp,
				Identity:    true,
				Message:     fmt.Sprintf("%s changed", kp),
			})
			continue
		}
		// No pre-composition source at all: a Pix-managed singleton facet
		// (schemaVersion/agent/name, or a template/pullPolicy override).
		// It is still identity-bearing — there is exactly one of it — so
		// this is never the identityless-list branch, only a differently
		// worded attribution.
		drifts = append(drifts, Drift{
			ComposedKey: key,
			Identity:    true,
			Message:     fmt.Sprintf("%s changed (pix-managed, no pre-composition source)", key),
		})
	}
	for list, keys := range listGroups {
		n := len(keys)
		drifts = append(drifts, Drift{
			ComposedKey:    list + "[]",
			Identity:       false,
			EntriesChanged: n,
			Message:        fmt.Sprintf("%s[] (%d entries changed)", list, n),
		})
	}
	sort.Slice(drifts, func(i, j int) bool { return drifts[i].ComposedKey < drifts[j].ComposedKey })
	return drifts
}

// ResetInvalidatedDrift is the ONE attributed drift record a caller
// synthesizes when ComputeFingerprint(current) fails with
// ErrHMACKeyMissing, instead of a per-interpolation or per-key refusal: a
// reset that took the launcher HMAC key away invalidates the WHOLE
// environment's creation acceptance at once. Recording that as N separate
// drift entries — one per authored ${VAR} reference (or worse, one per
// changed composed key) the environment happens to declare — would let a
// single reset on ONE environment flood recreatelog's bounded 100-record
// cap (I4), crowding out every other host's recreate history for
// something that was never really N independent drifts.
func ResetInvalidatedDrift() Drift {
	return Drift{
		ComposedKey: "*",
		Identity:    true,
		Message:     "acceptance invalidated by reset; recreate required",
	}
}

// diffFingerprintKeys returns every composed key present in either
// fingerprint whose value differs (an added/removed key counts as
// differing too), sorted for stable, deterministic output.
func diffFingerprintKeys(stored, current Fingerprint) []string {
	seen := map[string]bool{}
	var out []string
	for k, sv := range stored {
		seen[k] = true
		if cv, ok := current[k]; !ok || cv != sv {
			out = append(out, k)
		}
	}
	for k := range current {
		if seen[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// indexedListName reports the list name ("kits", "mounts") when key is
// exactly one identityless index-addressed entry of it.
func indexedListName(key string) (string, bool) {
	m := indexedListRE.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// identityKeyPaths collects every stable, identity-addressed KeyPath from
// pre's pre-composition Tree — everything EXCEPT Kits, which is
// index-addressed and handled by indexedListName instead (doc.go's
// "Stable identity").
func identityKeyPaths(pre *Tree) []string {
	if pre == nil {
		return nil
	}
	var out []string
	for _, n := range pre.SandboxOptions {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.Env {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.Secrets {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.Registries {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.BindingDomains {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.MCPServers {
		out = append(out, n.KeyPath)
	}
	for _, n := range pre.Ports {
		out = append(out, n.KeyPath)
	}
	return out
}

// findKeyPath finds the pre-composition KeyPath a composed key belongs to:
// an exact match for a leaf facet (secrets.<name>.ref, sandboxOptions.<key>,
// bindings...domains[<d>]), or the LONGEST matching "<KeyPath>." prefix for
// a composed sub-field of an identity-addressed node (mcp.servers[<name>]
// .url, ports[<n>].host).
func findKeyPath(identity []string, composedKey string) (string, bool) {
	for _, kp := range identity {
		if kp == composedKey {
			return kp, true
		}
	}
	best := ""
	for _, kp := range identity {
		if strings.HasPrefix(composedKey, kp+".") && len(kp) > len(best) {
			best = kp
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEffectiveSecretKeys(m map[string]effectiveSecret) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEffectiveRegistryKeys(m map[string]effectiveRegistry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBindingMapKeys(m map[string]Binding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
