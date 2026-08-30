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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/hosttrust"
	"pix/host/inference"
	"pix/host/lease"
	"pix/host/mcp"
	"pix/host/recreatelog"
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

	// PrimaryWorkspace is the run's OWN project workspace — `pix run DIR`,
	// or the current directory — never the selected environment's source
	// root. An environment root is a declaration directory, not a project:
	// docs/design/environments.md §5.1 restriction 4 requires it to resolve
	// OUTSIDE every writable workspace it mounts, so mounting it as the
	// project workspace would be exactly backwards. PrimaryWorkspaceFact is
	// the ONE producer of this fact and refuses an empty path outright, so
	// no launch can pick a workspace by empty-string accident.
	PrimaryWorkspace envinfo.WorkspaceFact
	PersonalContext  envinfo.WorkspaceFact
	// AdditionalWorkspaces are this launch's other RUNTIME host mounts
	// (workflow's MountDirs: configured skill trees, `--skills`, an active
	// pack's contributed skills/knowledge, `--dev`'s repo skills). After the
	// cutover `sbx env create` reads ONLY the effective document, so these
	// travel as workspace facts rather than as extra `sbx run` positionals.
	//
	// The environment's OWN authored `additionalWorkspaces:` are NOT copied
	// in here: envinfo renders them straight from Selection.Document, ahead
	// of these, and re-adding them would either duplicate the mount or push
	// a runtime read-write twin ahead of the authored read-only entry.
	// Envlaunch composes runtime mounts; the document composes its own.
	AdditionalWorkspaces []envinfo.WorkspaceFact

	// MixinKit is the generated Pi mixin kit REFERENCE (a directory this
	// launch already materialized). DevKit carries `--dev`'s checkout kit
	// and its live skill arguments. ExtraKits is every OTHER kit the old
	// argv passed as `--kit`: the base image kit, `--kit` overrides, an
	// active pack's generated mixin kits, the configured stack.
	MixinKit  string
	ExtraKits []string
	DevKit    envinfo.DevKitFact

	PixEnvVars map[string]string
	// MCPServers is the full create-time server set the effective document
	// declares. EnvMCPServers is the subset the ENVIRONMENT itself declares
	// (its `.sbxenv.yaml` servers plus their pix.toml credential wrappers) —
	// the only MCP facts an attach can recompute, and therefore the only
	// ones the creation fingerprint covers (see CreationFactsFor).
	MCPServers    []envinfo.MCPWrapperFact
	EnvMCPServers []envinfo.MCPWrapperFact
}

// PrimaryWorkspaceFact is the ONE producer of the primary project
// workspace fact. It refuses an empty path rather than substituting
// anything (an environment root, the process's cwd, or ""): the launch
// resolved a workspace before it ever got here, and a silently substituted
// one would mount the wrong tree read-write.
func PrimaryWorkspaceFact(workspace string) (envinfo.WorkspaceFact, error) {
	if strings.TrimSpace(workspace) == "" {
		return envinfo.WorkspaceFact{}, errors.New("launch: a primary workspace is required; refusing to compose one from an empty path")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return envinfo.WorkspaceFact{}, fmt.Errorf("launch: resolve workspace %q: %w", workspace, err)
	}
	return envinfo.WorkspaceFact{Path: abs}, nil
}

// WorkspaceFacts lifts an ordered list of host mount paths into workspace
// facts, dropping empties. Order is the caller's.
func WorkspaceFacts(paths []string) []envinfo.WorkspaceFact {
	var out []envinfo.WorkspaceFact
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		out = append(out, envinfo.WorkspaceFact{Path: abs})
	}
	return out
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
		AdditionalWorkspaces:     in.AdditionalWorkspaces,
		ExtraKits:                in.ExtraKits,
		MixinKit:                 in.MixinKit,
		DevKit:                   in.DevKit,
		PixEnvVars:               envVars,
		MCPServers:               in.MCPServers,
	}
}

// CreationFactsFor is the facts value the CREATION FINGERPRINT is computed
// over, on BOTH the create and the attach path, and it is deliberately a
// SUBSET of what ComposeRuntimeFacts renders.
//
// §10.2 requires an attach to recompute this fingerprint and compare it to
// the recorded one. An attach resolves NO create-only flag (run_cmd's own
// contract: nothing in the create-only block is even resolved on an
// attach), so any fact only a create resolves — the pinned image tag, the
// freshly materialized mixin-kit temp directory, an active pack's per-run
// generated kits and mounts, the host-global static-MCP name set — would
// differ on every attach and refuse every second `pix run`. Those facts are
// NOT left unguarded: the create-time session fingerprint
// (SessionFingerprint: the static MCP set and the image) still refuses an
// attach whose MCP set or image moved, under the lifecycle lock, exactly as
// it did before the cutover.
//
// What stays in, because both paths compute it identically from state that
// outlives one launch: the authored environment (document + sidecar), the
// pre-composition sandbox identity, the pinned pull policy, the project and
// personal-context workspaces, Pix env vars, and the environment's own
// declared MCP servers with their reviewed credential wrappers.
//
// Clearing AdditionalWorkspaces clears only the RUNTIME mounts (a pack's
// per-run directories, which move between launches). The environment's own
// authored `additionalWorkspaces:` stay covered, because they are rendered
// from Document — which this subset keeps — and an attach recomputes them
// from the same file.
func CreationFactsFor(in EffectiveInput) envinfo.RuntimeFacts {
	facts := ComposeRuntimeFacts(in)
	facts.Template = ""
	facts.ExtraKits = nil
	facts.MixinKit = ""
	facts.DevKit = envinfo.DevKitFact{}
	facts.AdditionalWorkspaces = nil
	facts.MCPServers = in.EnvMCPServers
	return facts
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
	if in.PrimaryWorkspace.Path == "" {
		return EffectiveEnvironment{}, errors.New("launch: refusing to render an effective environment with no primary workspace")
	}
	facts := ComposeRuntimeFacts(in)
	data, err := envinfo.RenderEffective(facts)
	if err != nil {
		return EffectiveEnvironment{}, err
	}
	fp, reset, err := CreationFingerprint(CreationFactsFor(in), resolve)
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

// AttachHMACResolver is the ATTACH path's launcher-keyed interpolation
// resolver: an authored ${VAR} is fingerprinted as an HMAC of its RESOLVED
// value under the one ALREADY STORED launcher key — never the raw value,
// never an unkeyed hash, and never a freshly generated key. It LOADS only:
// a missing key record is the state `pix reset` leaves behind for records
// written BEFORE the reset, and mapping hosttrust's sentinel onto envinfo's
// is the ONLY way the single reset-invalidated attribution is reached.
func AttachHMACResolver(configDir string, lookupEnv func(string) (string, bool)) envinfo.InterpolationResolver {
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
		return hosttrust.SignResolvedValue(key, resolveInterpolation(lookupEnv, varName, def)), nil
	}
}

// CreateHMACResolver is the CREATE path's resolver. A create is the moment
// the launcher's creation-fingerprint key comes into existence, so the key
// is ENSURED — generated once, under hosttrust's own lock, at 0600 — BEFORE
// the first fingerprint this launch computes, and captured for every facet
// afterwards. Two consequences are load-bearing:
//
//   - Exactly ONE ensure per launch, before anything is fingerprinted or
//     created. Ensuring lazily inside the closure would re-enter the lock
//     once per interpolated facet.
//   - A fresh host's FIRST interpolated create is a normal create, not a
//     "reset invalidated" one. The reset attribution belongs to records
//     written before the key went away — an attach — and a create that
//     cannot establish a key fails outright rather than silently
//     fingerprinting an environment it could not key.
func CreateHMACResolver(configDir string, lookupEnv func(string) (string, bool)) (envinfo.InterpolationResolver, error) {
	if strings.TrimSpace(configDir) == "" {
		return nil, errors.New("launch: cannot establish the creation fingerprint key without a config dir")
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	key, err := hosttrust.EnsureCreationHMACKey(configDir)
	if err != nil {
		return nil, err
	}
	return func(varName string, def *string) (string, error) {
		return hosttrust.SignResolvedValue(key, resolveInterpolation(lookupEnv, varName, def)), nil
	}, nil
}

// resolveInterpolation is the ${VAR} / ${VAR:-default} lookup both
// resolvers share. The value it returns is handed straight to
// hosttrust.SignResolvedValue and never travels anywhere else.
func resolveInterpolation(lookupEnv func(string) (string, bool), varName string, def *string) string {
	if value, ok := lookupEnv(varName); ok {
		return value
	}
	if def != nil {
		return *def
	}
	return ""
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

// RecordAttachRefusal appends exactly one recreatelog record (E1.6's
// bounded I4 diagnostic) when decision refused an attach because the
// environment's creation fingerprint drifted (docs/design/
// environments.md §9.4) — the ONLY §10.2 condition DecideEnvAttach
// attaches a non-empty Drifts to. Every OTHER refusal reason (not a
// schema-verified running sandbox, a different instance, no longer
// reviewed) has nothing drifted to log and is unrelated recreate-boundary
// tracking: this appends nothing for those, and nothing at all for a
// successful attach.
//
// envName is the recreatelog record's environment field; an empty name
// (the `none` state — no environment selected) has nothing to key a
// recreate-boundary record by, so it is skipped quietly rather than
// handed to recreatelog, which would otherwise refuse it outright.
//
// This is a diagnostic side-channel, never a reason `pix run` fails
// differently: a write error here (a wedged lock, an unwritable state
// dir) is returned to the caller to log at its own discretion, but must
// never change or block the refusal it is describing.
func RecordAttachRefusal(dir, envName string, decision AttachDecision) error {
	if decision.Attach || len(decision.Drifts) == 0 {
		return nil
	}
	if strings.TrimSpace(envName) == "" {
		return nil
	}
	paths := make([]string, 0, len(decision.Drifts))
	for _, d := range decision.Drifts {
		paths = append(paths, changedKeyPathForDrift(d))
	}
	return recreatelog.Append(dir, envName, paths)
}

// changedKeyPathForDrift is recreatelog's canonical changed-path for one
// attributed envinfo.Drift: the PRE-composition KeyPath Attribute traced
// (identity-bearing, e.g. "mcp.servers[github].url"), or — when no
// pre-composition source exists — the composed key itself: a Pix-managed
// singleton ("env.PIX_MEMORY_SCOPE changed (pix-managed, no
// pre-composition source)" collapses to its ComposedKey), a collapsed
// identityless-list group ("kits[]"/"mounts[]"), or ResetInvalidatedDrift's
// single "*" record. Every one of those is still a bare canonical key path
// recreatelog.Append accepts — never a facet value, never an argv, never a
// path outside the environment root.
func changedKeyPathForDrift(d envinfo.Drift) string {
	if d.KeyPath != "" {
		return d.KeyPath
	}
	return d.ComposedKey
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

// HolderProbeBudget bounds the ONE schema-verified listing every
// environment-holder question is answered from. An unbounded `sbx` here is
// what turns `pix env forget` or `pix env show` into a hang.
const HolderProbeBudget = health.StatusBudget

// EnvironmentHolders answers §10.4's live-holder question for ONE
// environment root: which sandboxes this host recorded against it are
// still positively live. It FAILS CLOSED — an sbx state it cannot read is
// an error, never "no holders" — because a wrong "nobody is holding it"
// is what turns `env forget` into a silent teardown of someone's session.
//
// It runs ONE bounded, SCHEMA-VERIFIED listing (`sbx ls --json`, parsed by
// package sandbox) for the whole answer, not a raw `sbx ls` per recorded
// sandbox: one timeout instead of N, one parse contract instead of column
// scraping, and "could not read the listing" is a single honest unknown
// rather than a per-sandbox guess.
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
	var recorded []string
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
		recorded = append(recorded, rec.SandboxName)
	}
	if len(recorded) == 0 {
		return nil, nil
	}
	listing, trusted := sbxListing(env, HolderProbeBudget)
	if !trusted {
		return nil, fmt.Errorf("launch: could not read a schema-verified `sbx ls` within %s; refusing to answer as unheld", HolderProbeBudget)
	}
	var held []string
	for _, name := range recorded {
		entry := sandbox.FindByName(listing, name)
		if entry == nil {
			continue // positively absent: nothing holds it
		}
		if !entry.IdentityVerified {
			return nil, fmt.Errorf("launch: `sbx ls` did not positively identify %s; refusing to answer as unheld", name)
		}
		if entry.State == sandbox.StateRunning {
			held = append(held, name)
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

// creationFingerprintFileName is the recorded CREATION fingerprint (§9.1),
// kept beside the lease as its own record rather than inside the session
// fingerprint: the two answer different questions ("did the environment
// declaration change" vs "did the static MCP set or image change") and are
// computed from different inputs, and folding them into one file would
// make an attach compare an environment fingerprint against a session one.
const creationFingerprintFileName = "creationfingerprint.json"

// RecordCreationFingerprint persists the creation fingerprint this create
// is FOR, beside the create intent and before `sbx env create` runs, so a
// later attach has something to compare against even if this process dies
// mid-create.
func RecordCreationFingerprint(sessionKey string, fp sandbox.Fingerprint) error {
	return writeSessionState(sessionKey, creationFingerprintFileName, fp)
}

// ReadCreationFingerprint reads it back. found is false when nothing was
// ever recorded (a pre-cutover sandbox), which is NOT drift.
func ReadCreationFingerprint(sessionKey string) (sandbox.Fingerprint, bool) {
	var fp sandbox.Fingerprint
	if !readSessionState(sessionKey, creationFingerprintFileName, &fp) || len(fp) == 0 {
		return nil, false
	}
	return fp, true
}

// ReleaseEffectiveEnv implements §10.3's "State and the effective file
// clear only after a positive absent probe": the retained effective
// document for sandboxName is deleted ONLY when ONE bounded,
// schema-verified listing positively reports no such sandbox. A listing it
// could not read, or one that still shows the sandbox, RETAINS the file —
// removal is the one thing that cannot be undone, and the file is what a
// later environment-scoped removal recomposes.
func ReleaseEffectiveEnv(env hostenv.Env, sandboxName string) (released bool, err error) {
	dir, derr := EffectiveEnvDir(sandboxName)
	if derr != nil {
		return false, derr
	}
	listing, trusted := sbxListing(env, HolderProbeBudget)
	if !trusted {
		return false, fmt.Errorf("launch: could not read a schema-verified `sbx ls` within %s; retaining %s's effective environment file", HolderProbeBudget, sandboxName)
	}
	if sandbox.FindByName(listing, sandboxName) != nil {
		return false, nil // still there: retain
	}
	if rerr := os.RemoveAll(dir); rerr != nil {
		return false, rerr
	}
	return true, nil
}

// EnvExtraKits is the launch's non-authored kit list, composed from the
// SAME inputs and in the SAME order BuildSbxArgs emitted `--kit` for
// before the cutover: the base image kit (a `--kit` override replaces it),
// the user's `--kit` values, the pack/generated mixin kits beyond the Pi
// mixin itself, then the configured stack. It exists because `sbx env
// create` reads only the effective document — a kit that used to travel as
// an argv flag has nowhere else to go, and dropping it would quietly
// unmount an active pack's contribution.
func EnvExtraKits(cfg *config.Config, o RunOpts, version string) []string {
	var kits []string
	if len(o.Kits) == 0 {
		if o.LocalKit != "" {
			kits = append(kits, o.LocalKit)
		} else {
			kits = append(kits, gitKitURLRef(o.KitRef, version))
		}
	}
	kits = append(kits, o.Kits...)
	if len(o.PackKits) > 1 {
		// PackKits[0] is the generated Pi mixin kit (RuntimeFacts.MixinKit);
		// the rest stack as ordinary extra kits.
		kits = append(kits, o.PackKits[1:]...)
	}
	if cfg != nil {
		kits = append(kits, cfg.Kits.Stack...)
	}
	return kits
}

// EnvMCPWrapperFacts composes §9.2's reviewed MCP facts for the SELECTED
// environment's own `.sbxenv.yaml` servers: a local-command server whose
// pix.toml `[host.mcp.<name>]` declares env_keys is wrapped through the ONE
// op-run grammar this module has (package mcp's OpRunWrap — never a second
// hand-built copy, arch_effective_test.go's
// TestArchitecture_NoDuplicateOpRunGrammar), and every other server renders
// its bare definition unchanged.
//
// It deliberately mirrors workflow/env's preview composition rather than
// calling it: workflow/launch may not import a sibling workflow package
// (F17), and the shared, non-duplicable part — the credential wrapper
// grammar itself — is the mcp.OpRunWrap call both go through.
func EnvMCPWrapperFacts(doc *envinfo.Document, sidecar *envinfo.Sidecar) []envinfo.MCPWrapperFact {
	if doc == nil {
		return nil
	}
	var hostMCP map[string]envinfo.HostMCPEntry
	if sidecar != nil {
		hostMCP = sidecar.Host.MCP
	}
	var out []envinfo.MCPWrapperFact
	for _, srv := range doc.MCP.Servers {
		fact := envinfo.MCPWrapperFact{Name: srv.Name, URL: srv.URL}
		if srv.Command == "" {
			out = append(out, fact)
			continue
		}
		argv := append([]string{srv.Command}, srv.Args...)
		if entry, ok := hostMCP[srv.Name]; ok && len(entry.EnvKeys) > 0 {
			argv = opRunWrapIfAvailable(argv)
		}
		fact.Command = argv[0]
		fact.Args = argv[1:]
		out = append(out, fact)
	}
	return out
}

// opRunWrapIfAvailable wraps argv through mcp.OpRunWrap when this host has
// both `op` and a refs file, and returns it unchanged otherwise (1Password
// stays optional — OpRunWrap's own no-op contract, reused rather than
// reimplemented).
func opRunWrapIfAvailable(argv []string) []string {
	opPath, err := exec.LookPath("op")
	if err != nil {
		return argv
	}
	refs := config.OpRefsPath()
	if _, serr := os.Stat(refs); serr != nil {
		return argv
	}
	return mcp.OpRunWrap(opPath, refs, argv)
}

// ComposeMCPServerFacts folds the host-global server names this create
// preloads (the pre-cutover `--static-mcp` set: configured servers, `--mcp`
// flags, an active pack's contribution, the ephemeral UAT server) into the
// environment's own declared servers, by NAME only.
//
// A host-global server is registered with, and run by, the sbx gateway
// (AGENTS.md's MCP rules); Pix holds no command or URL for it and must not
// invent one — so it travels as the stable identity it actually has. A name
// the environment already declares is never duplicated: the environment's
// own definition wins, since that is the one host review accepted.
func ComposeMCPServerFacts(envServers []envinfo.MCPWrapperFact, staticNames []string) []envinfo.MCPWrapperFact {
	seen := map[string]bool{}
	out := append([]envinfo.MCPWrapperFact(nil), envServers...)
	for _, s := range out {
		seen[s.Name] = true
	}
	for _, name := range staticNames {
		n := strings.TrimSpace(name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, envinfo.MCPWrapperFact{Name: n})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
