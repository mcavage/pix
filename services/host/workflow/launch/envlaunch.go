// envlaunch.go — E2.5's launch-side environment cutover: the ONE place a
// live `pix run` composes envinfo.RuntimeFacts, renders them through the
// single effective-document producer (envinfo.RenderEffective), persists
// the exact bytes it is about to hand `sbx env create`, and decides
// create-vs-attach from the creation fingerprint those same facts produce.
//
// Three boundaries this file holds, because breaking any of them recreates
// a bug the delivery already paid for:
//
//   - ONE producer. `pix env show --effective` (workflow/env's
//     ComputeEffective) and this file both build a RuntimeFacts value and
//     both render it through envinfo.RenderEffective. There is no second
//     renderer and no second effective grammar; the ONLY documented
//     difference is the launch-only computable facts a preview cannot
//     honestly invent (the actual pix-* sandbox name, the resolved image
//     tag, a materialized mixin kit path).
//   - ONE identity, attributed BEFORE composition (docs/design/
//     environments.md §6.2). The caller computes the pix-* sandbox name
//     from the workspace, hands it in as a fact, and that name — never
//     anything the authored document declares — names the effective file
//     and every later probe.
//   - Planning is inert. Nothing here execs sbx, removes a sandbox, or
//     writes machine config. It renders bytes, persists them atomically
//     under launcher-owned state, and returns argv/decisions for the
//     command layer to run through the existing proof-gated seams.
package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/hosttrust"
	"pix/host/inference"
	"pix/host/lease"
	"pix/host/sandbox"
	"pix/host/sys"
)

// EnvSelection is the resolved environment a launch runs under. The zero
// value is D17's `none` state: no environment is registered or selected,
// and the effective document is composed from Pix's own built-in defaults
// (§6.2) rather than refused.
type EnvSelection struct {
	// Name is the registered environment name, "" for `none`.
	Name string
	// Root is that environment's canonical root, "" for `none`.
	Root string
	// Document is the parsed native `.sbxenv.yaml`; nil for `none`, where
	// ComposeRuntimeFacts substitutes the built-in defaults document.
	Document *envinfo.Document
	// Sidecar is the optional parsed pix.toml, nil when absent.
	Sidecar *envinfo.Sidecar
	// Tree is the PRE-composition semantic tree drift attribution maps
	// composed keys back to (E2.2). nil is valid: attribution then falls
	// back to composed keys, never to a hash-only message.
	Tree *envinfo.Tree
	// Reviewed reports that host review is currently accepted for Root.
	Reviewed bool
}

func (s EnvSelection) Selected() bool { return strings.TrimSpace(s.Name) != "" }

// EffectiveInput is every fact a launch resolves for itself before this
// file composes anything. It exists so the composition is a pure function
// of already-resolved values: no *config.Config, no live probe, no clock.
type EffectiveInput struct {
	Selection EnvSelection

	// SandboxName is the pre-composition pix-* identity (§6.2).
	SandboxName string
	// Template is the exact image reference this launch pins (a resolved
	// `<repo>:<tag>`, or just the repo when no local tag is pinned), and
	// PullPolicy is §6.2's `missing`.
	Template   string
	PullPolicy string

	PrimaryWorkspace envinfo.WorkspaceFact
	PersonalContext  envinfo.WorkspaceFact

	// MixinKit is the generated Pi mixin kit REFERENCE (a directory this
	// launch already materialized). DevKit carries `--dev`'s checkout kit
	// and its live skill arguments.
	MixinKit string
	DevKit   envinfo.DevKitFact

	PixEnvVars map[string]string
	MCPServers []envinfo.MCPWrapperFact
}

// EffectivePullPolicy is §6.2's pinned pull policy. A launch never leaves
// it to sbx's default: a `local-*` tag that is never published must not
// send sbx to a registry.
const EffectivePullPolicy = "missing"

// EffectiveEnvFileName is the stable per-sandbox effective document name.
// Create and remove always use this same path (§6.2), which is why
// PlanEnvRemoveSeam can recompute it from the sandbox name alone.
const EffectiveEnvFileName = "effective.sbxenv.yaml"

// builtinDefaultsDocument is D17's `none` document: Pix's own built-in
// defaults, expressed as the minimum loadable native document. Every
// Pix-owned runtime fact (template, workspaces, kits, env) is layered on
// by RenderEffective from RuntimeFacts, so this carries no policy of its
// own — it exists so `none` renders a real document instead of erroring
// with envinfo.ErrNoDocument.
func builtinDefaultsDocument() *envinfo.Document {
	return &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1}
}

// ComposeRuntimeFacts is the LAUNCH's single RuntimeFacts producer.
func ComposeRuntimeFacts(in EffectiveInput) envinfo.RuntimeFacts {
	doc := in.Selection.Document
	if doc == nil {
		doc = builtinDefaultsDocument()
	}
	pull := in.PullPolicy
	if pull == "" {
		pull = EffectivePullPolicy
	}
	envVars := in.PixEnvVars
	if envVars == nil {
		envVars = map[string]string{}
	}
	return envinfo.RuntimeFacts{
		Document:                 doc,
		Sidecar:                  in.Selection.Sidecar,
		SandboxName:              in.SandboxName,
		Template:                 in.Template,
		PullPolicy:               pull,
		PrimaryWorkspace:         in.PrimaryWorkspace,
		PersonalContextWorkspace: in.PersonalContext,
		MixinKit:                 in.MixinKit,
		DevKit:                   in.DevKit,
		PixEnvVars:               envVars,
		MCPServers:               in.MCPServers,
	}
}

// EffectiveEnvDir/EffectiveEnvPath resolve the launcher-owned state
// location of one sandbox's effective document. State, not config: a
// moved config dir can never orphan a running sandbox from the file its
// own removal path recomposes (safety invariant 4).
func EffectiveEnvDir(sandboxName string) (string, error) {
	if strings.TrimSpace(sandboxName) == "" {
		return "", errors.New("launch: effective environment needs a sandbox name")
	}
	if !strings.HasPrefix(sandboxName, sandbox.Prefix) {
		return "", fmt.Errorf("launch: %q is outside the %s* namespace", sandboxName, sandbox.Prefix)
	}
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "environments", sandboxName), nil
}

func EffectiveEnvPath(sandboxName string) (string, error) {
	dir, err := EffectiveEnvDir(sandboxName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, EffectiveEnvFileName), nil
}

// PersistEffectiveEnv writes the EXACT bytes a create is about to use,
// atomically (same-directory temp + rename), so a reader — including this
// host's own removal path — never observes a half-written document and a
// crash mid-write leaves the previous document or nothing, never a torn
// mix. It returns the path it wrote.
func PersistEffectiveEnv(sandboxName string, data []byte) (string, error) {
	dir, err := EffectiveEnvDir(sandboxName)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("launch: create effective environment dir: %w", err)
	}
	path := filepath.Join(dir, EffectiveEnvFileName)
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("launch: write effective environment %s: %w", path, err)
	}
	return path, nil
}

// EffectiveEnvironment is one composed, rendered, persisted environment:
// the facts, the exact bytes, the path they live at, and the creation
// fingerprint an attach must later match.
type EffectiveEnvironment struct {
	Facts       envinfo.RuntimeFacts
	Bytes       []byte
	Path        string
	Fingerprint sandbox.Fingerprint
	// ResetInvalidated reports that the launcher HMAC key was gone when
	// this fingerprint was computed (E2.2): the whole environment's
	// acceptance is invalidated at once, and a caller attributes that as
	// ONE reset drift, never N per-interpolation ones.
	ResetInvalidated bool
}

// RenderEffectiveEnvironment composes, renders and persists in ONE step,
// so no caller can render one set of bytes and persist another.
func RenderEffectiveEnvironment(in EffectiveInput, resolve envinfo.InterpolationResolver) (EffectiveEnvironment, error) {
	facts := ComposeRuntimeFacts(in)
	data, err := envinfo.RenderEffective(facts)
	if err != nil {
		return EffectiveEnvironment{}, err
	}
	fp, reset, err := CreationFingerprint(facts, resolve)
	if err != nil {
		return EffectiveEnvironment{}, err
	}
	path, err := PersistEffectiveEnv(in.SandboxName, data)
	if err != nil {
		return EffectiveEnvironment{}, err
	}
	return EffectiveEnvironment{Facts: facts, Bytes: data, Path: path, Fingerprint: fp, ResetInvalidated: reset}, nil
}

// CreationFingerprint computes the recreate-only creation fingerprint over
// the SAME facts RenderEffective composed, adapted into the
// sandbox.Fingerprint shape the existing Diff/Equal engine already
// compares. A resolver reporting envinfo.ErrHMACKeyMissing is NOT an
// error: it is the reset-invalidated state, reported through the bool.
func CreationFingerprint(facts envinfo.RuntimeFacts, resolve envinfo.InterpolationResolver) (sandbox.Fingerprint, bool, error) {
	fp, err := envinfo.ComputeFingerprint(facts, resolve)
	if err != nil {
		if errors.Is(err, envinfo.ErrHMACKeyMissing) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return sandbox.FromFacetMap(fp), false, nil
}

// CreationHMACResolver is the launcher-keyed interpolation resolver E2.2
// requires: an authored ${VAR} is fingerprinted as an HMAC of its RESOLVED
// value under the one stored launcher key — never the raw value, never an
// unkeyed hash. A missing key record (the state `pix reset` leaves behind)
// maps hosttrust's sentinel onto envinfo's, which is the ONLY way the
// reset-invalidated attribution can be reached.
func CreationHMACResolver(configDir string, lookupEnv func(string) (string, bool)) envinfo.InterpolationResolver {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return func(varName string, def *string) (string, error) {
		key, err := hosttrust.LoadCreationHMACKey(configDir)
		if err != nil {
			if errors.Is(err, hosttrust.ErrCreationHMACKeyMissing) {
				return "", envinfo.ErrHMACKeyMissing
			}
			return "", err
		}
		value, ok := lookupEnv(varName)
		if !ok {
			if def != nil {
				value = *def
			} else {
				value = ""
			}
		}
		return hosttrust.SignResolvedValue(key, value), nil
	}
}

// EnvCreateArgs is the ONE create argv this launcher composes:
// `sbx env create <effective>`. There is deliberately no second,
// selectable create shape (PRD §8) — every create goes through the stable
// effective document, so create and remove name the same file.
func EnvCreateArgs(effectivePath string) []string {
	return []string{"env", "create", effectivePath}
}

// EnvRecreateGuidance is the ONE recovery command for an environment
// launch that cannot attach: remove the sandbox by its resolved name, then
// re-run naming the ENVIRONMENT. It never emits `--name`: a user who never
// typed one must not be taught to, and re-running with the same --env
// re-derives the same pix-* name from the workspace anyway.
func EnvRecreateGuidance(sandboxName, envName string) string {
	if strings.TrimSpace(envName) == "" {
		return fmt.Sprintf("pix rm %s && pix run", sys.ShellQuote(sandboxName))
	}
	return fmt.Sprintf("pix rm %s && pix run --env %s", sys.ShellQuote(sandboxName), sys.ShellQuote(envName))
}

// AttachGate is everything §10.2 requires before a name-based
// `sbx exec -it <name>` may run.
type AttachGate struct {
	// Entry is the schema-verified RUNNING row, or nil when sbx reported
	// no such row (absent, stopped, unverified, or unreadable — all of
	// which refuse: an unknown listing authorizes nothing).
	Entry *sandbox.Entry
	// RecordedInstanceID is the instance id this launcher recorded at
	// create time; "" means nothing was ever recorded.
	RecordedInstanceID string
	// Stored/Current are the recorded and freshly computed creation
	// fingerprints. A missing stored fingerprint (found == false) is not
	// drift: nothing was ever recorded to diverge from.
	Stored      sandbox.Fingerprint
	StoredFound bool
	Current     sandbox.Fingerprint
	// ResetInvalidated is CreationFingerprint's reset bit.
	ResetInvalidated bool
	// Reviewed reports the environment is STILL reviewed.
	Reviewed bool
	// Tree is the pre-composition tree drift is attributed against.
	Tree *envinfo.Tree
}

// AttachDecision is the gate's verdict. Refusal is a complete, already
// worded message; Drifts is the attributed facet list behind it.
type AttachDecision struct {
	Attach  bool
	Refusal string
	Drifts  []envinfo.Drift
}

// DecideEnvAttach refuses on ANY of §10.2's four conditions, in the order
// a human debugs them: is it actually running and verified, is it the
// instance we created, does its creation fingerprint still match, is the
// environment still reviewed.
func DecideEnvAttach(g AttachGate, sandboxName, envName string) AttachDecision {
	guidance := EnvRecreateGuidance(sandboxName, envName)
	refuse := func(reason string, drifts []envinfo.Drift) AttachDecision {
		var b strings.Builder
		fmt.Fprintf(&b, "%q %s — refusing to attach.\n", sandboxName, reason)
		for _, d := range drifts {
			fmt.Fprintf(&b, "     drifted: %s\n", d.Message)
		}
		fmt.Fprintf(&b, "     recreate it: %s", guidance)
		return AttachDecision{Refusal: b.String(), Drifts: drifts}
	}

	switch {
	case g.Entry == nil:
		return refuse("is not a schema-verified running sandbox", nil)
	case !g.Entry.IdentityVerified || g.Entry.State != sandbox.StateRunning:
		return refuse("is not a schema-verified running sandbox", nil)
	}
	live := ""
	if g.Entry.InstanceID != nil {
		live = *g.Entry.InstanceID
	}
	switch {
	case g.RecordedInstanceID == "":
		return refuse("has no recorded creation instance on this host", nil)
	case live == "" || live != g.RecordedInstanceID:
		return refuse("is a different instance than the one this host created", nil)
	}
	if g.ResetInvalidated {
		return refuse("no longer matches its recorded creation fingerprint", []envinfo.Drift{envinfo.ResetInvalidatedDrift()})
	}
	if g.StoredFound {
		if drifts := envinfo.Attribute(g.Tree, envinfo.Fingerprint(g.Stored), envinfo.Fingerprint(g.Current)); len(drifts) > 0 {
			return refuse("no longer matches its recorded creation fingerprint", drifts)
		}
	}
	if !g.Reviewed {
		return refuse("names an environment that is no longer reviewed", nil)
	}
	return AttachDecision{Attach: true}
}

// SelectSessionModel is §6.3's precedence, in one place: an explicit
// --model wins; otherwise the selected environment's `[models].main`;
// otherwise nothing at all, which means pi's own default. It never
// consults the router and never invents a model.
func SelectSessionModel(explicit string, sc *envinfo.Sidecar) (model, source string) {
	if m := strings.TrimSpace(explicit); m != "" {
		return m, "--model"
	}
	if sc != nil {
		if m := strings.TrimSpace(sc.Models.Main); m != "" {
			return m, "[models].main"
		}
	}
	return "", ""
}

// RosterInputFor lifts the selected environment's authored roster into the
// generated-manifest input (E3.1). A nil sidecar yields the zero value: no
// roster, which BuildRoster treats as "emit no additive roster key".
func RosterInputFor(sc *envinfo.Sidecar, shippedAgents []string) inference.RosterInput {
	if sc == nil {
		return inference.RosterInput{}
	}
	return inference.RosterInput{
		Main:          strings.TrimSpace(sc.Models.Main),
		Agents:        sc.Agents,
		ShippedAgents: shippedAgents,
	}
}

// sessionEnvironmentFileName records WHICH environment a live sandbox was
// created from, beside its lease record. It is what makes a later
// environment-scoped question ("is anything still holding `work`?")
// answerable without guessing, after PromoteCreateIntent has already
// cleared the create intent.
const sessionEnvironmentFileName = "environment.json"

// SessionEnvironment is that record: identity by ROOT (the same key
// hosttrust acceptance is subject to), with the name and effective path
// kept for a human and for the removal planner.
type SessionEnvironment struct {
	Name          string `json:"name,omitempty"`
	Root          string `json:"root,omitempty"`
	SandboxName   string `json:"sandbox_name"`
	EffectivePath string `json:"effective_path,omitempty"`
}

func RecordSessionEnvironment(sessionKey string, rec SessionEnvironment) error {
	return writeSessionState(sessionKey, sessionEnvironmentFileName, rec)
}

func ReadSessionEnvironment(sessionKey string) (SessionEnvironment, bool) {
	var rec SessionEnvironment
	if !readSessionState(sessionKey, sessionEnvironmentFileName, &rec) {
		return SessionEnvironment{}, false
	}
	return rec, true
}

// EnvironmentHolders answers §10.4's live-holder question for ONE
// environment root: which sandboxes this host recorded against it are
// still positively live. It FAILS CLOSED — an sbx state it cannot read is
// an error, never "no holders" — because a wrong "nobody is holding it"
// is what turns `env forget` into a silent teardown of someone's session.
func EnvironmentHolders(env hostenv.Env, envRoot string) ([]string, error) {
	root, err := leaseRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	want := strings.TrimSpace(envRoot)
	var held []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var rec SessionEnvironment
		data, rerr := os.ReadFile(filepath.Join(root, e.Name(), sessionEnvironmentFileName))
		if rerr != nil {
			continue
		}
		if jerr := json.Unmarshal(data, &rec); jerr != nil {
			continue
		}
		if rec.Root == "" || rec.Root != want || rec.SandboxName == "" {
			continue
		}
		switch ProbeTaskSandbox(env, rec.SandboxName) {
		case SbxRunning:
			held = append(held, rec.SandboxName)
		case SbxUnknown:
			return nil, fmt.Errorf("launch: could not determine whether %s is still running; refusing to answer as unheld", rec.SandboxName)
		}
	}
	sort.Strings(held)
	return held, nil
}

// EnvironmentHolderProbe adapts EnvironmentHolders to the name-keyed,
// fail-closed probe shape `env forget` takes (workflow/env's HolderProbe),
// with the caller supplying the name -> canonical root lookup, since only
// it holds the config.
func EnvironmentHolderProbe(env hostenv.Env, rootFor func(name string) (string, bool)) func(string) (bool, error) {
	return func(name string) (bool, error) {
		if rootFor == nil {
			return false, errors.New("launch: no environment root lookup; refusing to answer as unheld")
		}
		root, ok := rootFor(name)
		if !ok || strings.TrimSpace(root) == "" {
			return false, nil // not registered: nothing this host could have launched under it
		}
		held, err := EnvironmentHolders(env, root)
		if err != nil {
			return false, err
		}
		return len(held) > 0, nil
	}
}

// EnvTeardownPlanner is the planner a launch hands SessionDeps.Teardown so
// the environment-scoped removal path (E2.4) runs INSIDE the existing
// proof chain rather than beside it: PlanEnvRemoveSeam recomposes the same
// stable effective path, and TeardownSandbox still enforces every
// holder/keep/instance-id/fresh-probe proof before the argv is executed.
func EnvTeardownPlanner(sandboxName string) TeardownPlanner {
	return func(name string) (EnvRemovalPlan, error) {
		path, err := EffectiveEnvPath(sandboxName)
		if err != nil {
			// No resolvable state path: fall back to the name-based planner
			// with its own report, exactly as a missing file would.
			path = ""
		}
		return PlanEnvRemoveSeam(path, name, false)
	}
}

// WriteCreateIntentFor commits E2.3's bounded create intent for one
// session BEFORE `sbx env create` is spawned: which environment, which
// pix-* name, which fingerprint this create is FOR. A `none` launch (no
// registered environment root) writes no intent — there is no environment
// identity to record, and validateCreateIntent would rightly refuse a
// fabricated one — while every other proof on the create path is
// unchanged.
func WriteCreateIntentFor(sessionKey, envRoot, envName, sandboxName string, fp sandbox.Fingerprint) error {
	if strings.TrimSpace(envRoot) == "" {
		return nil
	}
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return err
	}
	return WriteCreateIntent(dir, CreateIntent{
		EnvironmentRoot: envRoot,
		EnvironmentName: envName,
		SandboxName:     sandboxName,
		Fingerprint:     fp,
	})
}

// ReadRecordedInstanceID reports the instance id this host recorded when it
// created sessionKey's sandbox, or "" when nothing was ever recorded.
func ReadRecordedInstanceID(sessionKey string) string {
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return ""
	}
	rec, rerr := lease.ReadRecord(dir)
	if rerr != nil {
		return ""
	}
	return rec.InstanceID
}

// ReadSessionFingerprintFor exposes the recorded creation fingerprint to
// the command layer's attach gate. found is false when nothing was ever
// recorded — which is NOT drift: there is no stored value to diverge from.
func ReadSessionFingerprintFor(sessionKey string) (sandbox.Fingerprint, bool) {
	return readSessionFingerprint(sessionKey)
}
