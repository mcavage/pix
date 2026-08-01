# `pix models` — one noun for inference, and a way back in after first run

Status: PROPOSED (design only; no code in this change)
Owner: architect
Supersedes: nothing. Extends `docs/design/routing.md` (the router itself is
unchanged) and `docs/design/onboarding-v3.md` (the first-run inference flow).
Touches safety invariants **1, 5, 6, 10, 13** (AGENTS.md). Each is called out
by number where it binds.
Revision 2: revised after a cross-vendor adversarial review returned BLOCK.

## Review findings folded in

What the reviewer contested, and what changed. Read this first if you are
implementing from a copy of revision 1.

| # | finding | resolution |
|---|---|---|
| 1 | **BLOCKER.** The widening algorithm computed its "seen" baseline from the bindings *after* `configureDirectInference` had already added the new provider, so on any legacy config (`roster_providers` absent — i.e. every install that exists today) `pix models add google` widened nothing, `verifyDirectInference` skipped google entirely, and the command printed success over a reproduced A3 dead end. | **Fixed.** The seen baseline is now captured PRE-mutation and passed into `configureModelRoster` (`prior`). See [Roster widening](#roster-widening). Tests 7/8 rewritten so the legacy `models add` path is the covered one. |
| 2 | The deprecation window was justified with two invented claims: that `TestHelpListsEveryTopLevelVerb` forces a `hiddenVerbs` entry, and that `extensions/subagents.ts` shells out to `pix route`. Both false. | **Fixed by retraction.** Both claims removed. The alias is now justified as a cheap, reversible courtesy, and the hard cut is documented as an equally defensible option the implementer may take. See [Deprecation](#deprecation-alias-for-one-release-as-a-courtesy-not-a-constraint). |
| 3 | The copy inventory claimed completeness but omitted four production `pix route` strings (`route.go:41`, `agent.go:333`, `:358`, `:395`), which the newly proposed guard would fail; `agent.go` was absent from the blast-radius table. | **Fixed.** All four (plus `route.go:1`) now have an old→new in [Copy changes](#copy-changes); `agent.go` added to the blast radius. |
| 4 | The "keys closure shrinks to" snippet deleted `setupChooseInference`, the `selected` branch, `ensureSetupPrereqsFor`, and `setupProvisionKeysFn`, which would hard-fail `pix setup` under a mandatory pack and contradict the doc's own text. | **Fixed.** The snippet now shows the real closure (`setup.go:665–716`) with only the tail (`:692–716`) replaced. |
| 5 | The A3 dead end survived in guidance the doc did not touch: `runIntentKeyCheck`'s todo (`doctor_providers.go:112`), `setup.go:1089`, `setup.go:1276`. The proposed copy guard banned only `pix route`. | **Fixed.** Copy changes 12–14 rewrite them; the copy guard gains a second ban with an explicitly stated blind spot (a concatenated key name is not a source literal), covered by a rendered-output test instead. |
| 6 | NIT: the `ExclusiveSource` refusal should tell a temporary-pack user that stashing the ref now still works. | **Fixed** in the refusal copy. |
| 7 | NIT: `models route` is the lone mutating subcommand in a read-only sibling set. | **Argued down, with mitigations.** The name honors the owner's literal ask; the wart is mitigated in three places (bare `Next:` line, usage annotation, no implicit compile) rather than renamed. See the note under [The verb tree](#the-verb-tree). |
| 8 | NIT: stale line refs. | **Fixed:** `setup.go:1387`→`:1391`, `inference.go:218`→`:289` (the block; the function is still `:218`), `inference.go:452`→ function `:452`, binding rebuild `~:470`, `readiness_types.go:94`→`:184` (`blockingCheck`) / `:273` (`outstanding`) / `:101` (the `note` field). |

Not re-litigated (the reviewer confirmed them): the three reconcile error
semantics at `setup.go:698–712`, the doctor verdict mechanics and the
`"optional todo never blocks"` row of `TestExitMatrix`
(`verbcoverage_test.go:119`), `man_test.go`'s strict 1:1 constraint
(`man_test.go:37–64` vs `pix.1:714–740`), `retiredVerbs` (`help.go:49`), and
the rationale for refusing rather than half-succeeding under an exclusive pack.

## Problem

Two complaints, verbatim from the product owner.

**A. The one-way door.**

> "I set this up to use you, and i picked api keys. I was forced to pick one of
> anthropic/openai, and setup told me i could set the rest up later. but for the
> life of me i dont know where or how in this cli. so that flow just seems
> wrong."

**B. The wrong noun.**

> "the 'route' command seems dumb. really we should just call it models, have a
> models and inference setup and have a models route command that sets all the
> intents -> model maps"

These are the same bug seen from two angles. There is no place in the CLI
called "models". So the thing you want to do second ("add my other key") has no
home, and the thing that *is* implemented (`route`) is filed under the
mechanism rather than the noun. A user who wants to change what models pix can
use has nowhere obvious to look, and the one path they might guess is a dead
end.

## What is broken today

### A1. Setup forces one provider and never names the way back

`services/host/cmd/pix/setup.go:1391` (inside `promptProviderChoice`,
declared at `:1389`):

```go
fmt.Fprintln(out, "One model provider is enough to start. You can add others later.")
fmt.Fprintln(out, "  1. openai (default)")
fmt.Fprintln(out, "  2. anthropic")
fmt.Fprintln(out, "  3. google")
```

"later" is never resolved to a command. This violates the CLI-redesign taste
rule #3 ("the tool teaches you as you use it; every screen ends with an obvious
next step") in the single place it matters most.

### A2. The only advertised later-path does not wire inference

Every "how to add a key" string in the tree points at `pix secret set` +
`pix secret sync`:

- `services/host/cmd/pix/doctor_providers.go:118` — `modelKeyFixCmd`
- `services/host/cmd/pix/doctor_providers.go:112` — `runIntentKeyCheck`'s todo
  (built by concatenation, so a grep for the literal misses it — revision 1
  missed it)
- `services/host/cmd/pix/run.go:851` — `modelKeyMissingMessage`
- `services/host/cmd/pix/secret_sync.go:748` — `runSecretSync`'s fatal arm
- `services/host/cmd/pix/setup.go:1089` — setup's `provider keys` core check

Both commands do a real job: `runSecretSetLocked`
(`services/host/cmd/pix/secret.go:305`) upserts `op-refs.env` and mirrors
provider keys into `hostmode.env`; `runSecretSync`
(`services/host/cmd/pix/secret_sync.go:748`) resolves refs into sbx secrets.
Neither one contains the string `Inference`. Confirmed by grep across both
files.

### A3. Consequence: a correctly-set key is completely inert

`configureDirectInference` (`services/host/cmd/pix/inference.go:452`; it wipes
and rebuilds every native binding from the catalog at `~:470`) is the
*only* function that turns a provider name into
`cfg.Inference.Backends[p]` + one `InferenceModelBinding` per catalog model.
It is called from exactly one place: the `keys` mutation step in
`setupMutationSteps` (`setup.go:659`; the step opens at `:662`, its `run`
closure spans `:665–716`, and the `configureDirectInference` call is at `:695`).

So after `pix secret set GEMINI_API_KEY op://...` + `pix secret sync`:

- `cfg.Inference.Backends` has no `google` entry
- `cfg.Inference.Models` has no `google/*` bindings
- `verifyDirectInference` never probes it (it iterates `cfg.Inference.Models`)
- `routing.json` is never recompiled, so no agent can route to it
- `resolveSessionModel` (`route.go`) will not resolve an intent to it, because
  "once backend bindings exist they are the availability authority"

The key is present in 1Password, present in `op-refs.env`, present in
`hostmode.env`, present in sbx — and unusable.

### A4. Re-running `pix setup` does not fix it

`configureModelRoster` (`inference.go:218`) freezes the roster at
`inference.go:289`:

```go
// Preserve an existing choice, dropping stale models that no longer have a
// binding. `--models` is the explicit way to change it on a later setup run.
if len(cfg.Inference.AllowedModels) > 0 {
    var kept []string
    for _, id := range cfg.Inference.AllowedModels {
        if bound[id] { kept = append(kept, id) }
    }
    if len(kept) > 0 { cfg.Inference.AllowedModels = kept; return nil }
}
```

The list only ever shrinks. It never learns that a second provider arrived. The
one escape is `pix setup --models <id,id,...>` — documented only inside
`setupUsage` (`setup.go:1673`), requiring the user to know catalog model ids by
hand. This is a dead end, not merely poor discoverability.

### B1. `route` is the mechanism, not the noun

`services/host/cmd/pix/route.go` is a 125-line passthrough: `runRoute` →
`execHost("route", argv)` → `pix-host route {pick|compile|show|models}`
(`services/host/route.go:18`). Help renders it at `help.go:143` under
"Models & agents (cost/latency/accuracy routing)", directly under `agent`. The
noun the user is looking for — **models** — is a *subcommand of a verb named
after the algorithm*. `pix route models` is exactly backwards.

## The verb tree

`pix models` becomes the noun. Seven subcommands; bare `pix models` is
read-only status (CLI-redesign taste rule #5: the bare command is safe).

| command | what it does | delegates to |
|---|---|---|
| `pix models` | status: inference runtime, which providers are bound, the roster, and any key-with-no-binding gap. Ends with a `Next:` line. Read-only. | none (launcher-local; reads `config.toml` + `hostmode.env`) |
| `pix models ls [--json]` | list the model registry (id, label, provider, context, price) | `pix-host route models` |
| `pix models show [--json]` | registry + scorecard + the resolved intent table | `pix-host route show` |
| `pix models pick <intent> [--json]` | resolve one intent to a model, with the rationale | `pix-host route pick` |
| `pix models route [--out PATH]` | resolve **every** intent and write the intent→model map (`routing.json`) that the sandbox reads | `pix-host route compile` |
| `pix models add [provider...] [--models ids] [--no-verify] [--yes]` | **Problem A.** Collect the 1Password ref for a provider, reconcile it into sbx, derive its backend + model bindings, widen the roster, live-probe, save. With no provider argument: reconcile whatever refs already exist. | none (launcher-local; see the reconcile seam) |
| `pix models setup` | re-ask *only* the inference question (`setupChooseInference` + `configureModelRoster` + verify), without a full `pix setup` | none (launcher-local) |

Notes on the shape:

- **`models route`, not `models compile`.** The owner's own words: "have a
  models route command that sets all the intents -> model maps". `route`
  survives as the *verb under the noun*, which is where it always belonged.
  `pix models compile` stays as an undocumented alias, because that spelling is
  in `skills/model-refresh/SKILL.md`, `extensions/subagents.ts`, `Makefile`,
  and every existing session's muscle memory. It maps to the same
  `pix-host route compile`.
- **Known wart: `models route` is the only mutating sibling.** `ls`, `show`,
  `pick` and bare `models` are read-only; `route` writes `routing.json`. That
  is a real inconsistency and the reviewer was right to flag it. It is
  **accepted, not renamed**, because the owner asked for this spelling by name
  ("have a models route command that sets all the intents -> model maps") and a
  design that overrules the one explicit naming request in the complaint is
  worse than a labelled wart. Three mitigations, all cheap: (a) the bare
  `pix models` `Next:` block labels it as the one that writes; (b)
  `modelsUsage()` annotates it `(writes routing.json)` while the other four
  carry no such tag; (c) it is never invoked implicitly — `models add` prints
  it as a next step rather than running it (open question 2). Rejected:
  `models compile` (clearer verb/effect match, but contradicts the ask) and
  `models route --write` (a flag to make the only behavior happen is noise).
- **`ls` not `models models`.** Renaming `route models` → `models ls` is what
  makes the noun rename pay for itself. `pix models ls` is the registry;
  `pix models` is your status. Same relationship as `pix agent ls` vs
  `pix agent`.
- **`pix-host route` does not move.** The host binary's subcommand tree
  (`services/host/route.go:18`) is internal plumbing, referenced by
  `pix-host route compile` in the Makefile and by maintainers. Renaming it buys
  nothing and costs a second migration. The launcher-facing noun is the only
  thing the user sees. The passthrough stays thin: `execHost("route", …)` with
  the launcher-side subcommand mapped to the host-side one.
- **Rejected: `models roster`.** A third spelling for something `models setup`
  (interactive) and `models add --models` (non-interactive) already do. YAGNI.
- **Rejected: `models rm <provider>`.** Nobody asked. Removing a key is
  `pix secret rm` + `pix models add` (reconcile), and the roster prunes stale
  entries automatically. Note the deferral on purpose; add it when someone
  actually needs it.

### `pix models` (bare) — the screen that answers the complaint

```
Inference                                        config: ~/.config/pix/config.toml

Runtime      direct provider keys (1Password)
Backends     anthropic  native   ANTHROPIC_API_KEY   4 model(s), 4 verified
             openai     native   OPENAI_API_KEY      2 model(s), 2 verified

Roster       anthropic/claude-opus-5, anthropic/claude-sonnet-5,
             anthropic/claude-haiku-4-5, openai/gpt-5.6-sol   (7 available)

Session      run_intent=overlord -> openai/gpt-5.6-sol

  ! GEMINI_API_KEY is set but google has no model bindings.
    Agents cannot use it. Wire it in:  pix models add google

Next:  pix models add google     wire the google key in
       pix models show           the full registry + resolved intents
       pix models route          rewrite routing.json (the only one here that writes)
```

That block is the entire fix for Problem A on the discovery axis. The user who
typed `pix models` because it is the obvious noun gets told exactly what is
wrong and exactly what to type.

## The reconcile seam

### Signature

Extract the **tail** of the `keys` mutation step (`setup.go:692–716`, from
`hostModeProviderKeys` through the partial-verify line) into one function. The
head of that closure — `setupChooseInference`, the GitHub credential sync, the
`selected` (gateway/ollama/pack) branch, `ensureSetupPrereqsFor`, and
`setupProvisionKeysFn` — **stays in setup**; it is setup-only sequencing and
prompt budgeting, and moving it is what would break `pix setup` under a
mandatory pack. Setup then calls the extracted tail, so there is one
implementation of the reconcile and no drift.

```go
// reconcileDirectInference re-derives the direct-provider inference surface
// from the provider-key op:// refs currently on disk (hostmode.env), and
// re-earns Verified with live probes. It is the tail of setup's `keys`
// mutation step, extracted so every path that can ADD a provider key converges
// on one implementation. It never reads or writes a secret VALUE to disk
// (invariant 10): op:// refs in, resolved key bytes in process memory only.
//
// Callers: setup's keys step, `pix models add`, `pix models setup`.
func reconcileDirectInference(cfg *config.Config, env shellEnv, out io.Writer, opts reconcileOpts) (reconcileResult, error)

type reconcileOpts struct {
	// models is an explicit roster override (the --models flag). Empty means
	// "apply the widening policy" (see Roster widening).
	models string
	// verify runs verifyDirectInference. False skips the live probes AND
	// suppresses every "verified" word in the caller's output (invariant 13).
	verify bool
	// interactive/in gate configureModelRoster's prompt, exactly as setup does.
	interactive bool
	in          io.Reader
}

type reconcileResult struct {
	Providers []string // provider names with a usable ref in hostmode.env
	// Added is Providers minus the providers that were ALREADY bound when the
	// call started (the `prior` baseline below), so it answers "what did this
	// call wire?" honestly. Do not derive it from the roster.
	Added     []string
	Roster    []string // cfg.Inference.AllowedModels after the call
	Attempted int
	Verified  int
	Failures  []string
}
```

### Body (setup's sequence, plus the pre-mutation baseline)

One helper is new:

```go
// boundNativeProviders returns the providers that currently have at least one
// binding on a native (direct-key) backend. Callers MUST call it before any
// mutation; see configureModelRoster's `prior` parameter.
func boundNativeProviders(cfg *config.Config) []string
```

```go
if cfg.Inference.ExclusiveSource != "" {
	return reconcileResult{}, errInferenceExclusive  // see Pack behavior
}
// CAPTURE THE BASELINE FIRST. `prior` is the set of providers that had native
// bindings BEFORE this call mutated anything. configureDirectInference wipes
// and rebuilds every native binding (inference.go:~470), so after it runs there
// is no way to tell a provider that was already wired from the one the user
// just asked for. Everything the widening policy decides hangs off this line.
prior := boundNativeProviders(cfg)                 // pre-mutation, do not move

providers, err := hostModeProviderKeys(env)
if err != nil { return res, fmt.Errorf("reading configured providers: %w", err) }
if err := configureDirectInference(cfg, providers); err != nil {
	return res, fmt.Errorf("configuring direct inference: %w", err)
}
res.Providers = providers
res.Added = subtract(providers, prior)              // NOT derived from the roster
// Roster BEFORE verify: verifyDirectInference only probes bindings that pass
// inferenceBindingAllowed, which requires membership in a non-empty
// AllowedModels. If the roster has not widened yet, the new provider's models
// are never probed, Verified counts only the old provider, and the command
// prints success over an unusable key. That ordering is invariant 13 load-
// bearing, not stylistic.
if err := configureModelRoster(cfg, opts.in, out, opts.interactive, opts.models, prior); err != nil {
	return res, fmt.Errorf("choosing models: %w", err)
}
if opts.verify {
	res.Attempted, res.Verified, res.Failures = verifyDirectInference(cfg, env)
}
if err := cfg.Save(); err != nil { return res, err }        // SAVE BEFORE the verify verdict
if opts.verify && res.Verified == 0 && (res.Attempted > 0 || len(res.Failures) > 0) {
	detail := strings.Join(res.Failures, "; ")
	if detail == "" { detail = "no provider accepted a model-specific request" }
	return res, fmt.Errorf("provider keys resolved, but live inference verification failed: %s", detail)
}
return res, nil
```

Three error semantics that are load-bearing and must not be "cleaned up":

1. **`cfg.Save()` happens before the `verified == 0` error return.** Setup does
   this today and it is correct: bindings are durable progress. A network blip
   during the probe must not throw away the config write, or re-running becomes
   the only recovery and the user is back in the dead end.
2. **`verified == 0` with attempts is a hard error.** Invariant 13: no success
   word without a probe. Nothing may print "ready" here.
3. **`verified > 0` with partial failures is NOT an error.** It prints the
   existing informational line
   (`"  inference: %d model(s) verified; %d candidate(s) unavailable or unauthorized (%s)"`)
   and returns nil. One unauthorized model (an account without Opus access) must
   not fail the whole command.

Three more ordering rules that follow from the same reasoning:

4. **`prior` is captured before `configureDirectInference`.** See BLOCKER 1 in
   the findings table. A reader who "tidies" this into
   `providersOf(cfg.Inference.Models)` at the point of use silently restores
   the A3 dead end behind a success message. Test 7 is the guard.
5. **`configureModelRoster` runs before `verifyDirectInference`.** As annotated
   above.
6. **`res.Added` is derived from `prior`, not from the roster.** It is the
   honest answer to "what did this call wire?" and it is what the `Next:` line
   and open question 2 key off.

`setupMutationSteps`'s keys closure keeps its head and replaces only its tail
(`setup.go:692–716`). Unchanged lines elided with `…`:

```go
run: func() error {
	selected, err := setupChooseInference(cfg, env, in, out, interactive)   // :666  UNCHANGED
	if err != nil { return err }
	if err := syncGitHubCredentialFromHost(env); err != nil { … }           // :672  UNCHANGED
	if selected {                                                          // :677  UNCHANGED
		// Pack/gateway/ollama supplied the backend. No provider keys, no
		// reconcile, no probe. `prior` here is the current bound set, so
		// the widening policy is a no-op on this path by construction.
		if err := configureModelRoster(cfg, in, out, interactive, opts.models,
			boundNativeProviders(cfg)); err != nil {
			return fmt.Errorf("choosing models: %w", err)
		}
		return cfg.Save()
	}
	if err := ensureSetupPrereqsFor(env, in, out, interactive, true); err != nil {
		return err                                                         // :683  UNCHANGED
	}
	if !setupProvisionKeysFn(env, in, out, interactive, opts.assumeYes) {   // :690  UNCHANGED
		return fmt.Errorf("provider keys not fully configured — follow the fix printed above")
	}

	// ---- :692–716 REPLACED BY THE SEAM ----
	res, err := reconcileDirectInference(cfg, env, out, reconcileOpts{
		models: opts.models, verify: true, interactive: interactive, in: in,
	})
	if err != nil { return err }
	if res.Verified > 0 && len(res.Failures) > 0 { /* the existing partial line */ }
	return nil
},
```

Two things that snippet is deliberately protecting, both of which a naive
"the closure becomes one call" extraction breaks:

- **Key provisioning stays in setup.** `setupProvisionKeysFn` is the only
  mutation allowed to write to the real terminal and it is prompt-budgeted;
  `reconcileDirectInference` reads refs that are already on disk and prompts
  for nothing. Folding the former into the latter would give `pix models add`
  setup's full interrogation.
- **Setup never hits `errInferenceExclusive`.** The keys step is `fatal: true`,
  so if the whole closure were the reconcile call, a mandatory pack (which
  makes `setupChooseInference` return `selected == true` and short-circuit
  today) would instead reach the exclusive-source guard and hard-fail
  `pix setup`. The `selected` branch above is what keeps that unreachable —
  which is what the Pack behavior section already claims, and now actually
  shows.

### Call sites, and what does NOT call it

| call site | reconciles? | verify | rationale |
|---|---|---|---|
| `setupMutationSteps` keys step | yes | true | unchanged behavior, now via the shared seam |
| `pix models add [provider...]` | yes | true (unless `--no-verify`) | the user explicitly asked to wire a provider in; the probe is the point |
| `pix models setup` | yes | true | same |
| `pix secret set <PROVIDER_KEY>` | **no** — prints a next step | — | see below |
| `pix secret sync` | **no** — prints a next step | — | see below |

**Why `secret set` must not reconcile automatically.** The principle is that a
command should not silently do a slow network probe the user did not ask for.
Concretely:

- `secret set`'s contract is a *file transaction*: upsert `op-refs.env`, mirror
  `hostmode.env`, under a lock. It is fast, offline, and deterministic.
  Reconciling turns it into N `op read` calls plus N concurrent HTTPS requests
  bounded by `directInferenceProbeTimeout` (8s, `inference.go:21`). A user
  typing three `secret set` calls in a row would trigger three probe storms.
- `secret set` is not provider-specific. It is the mechanism for
  `SLACK_TOKEN`, `GOG_*`, and pack credentials. A reconcile is meaningless for
  those, so the behavior would have to fork on the key name anyway — and a
  command that sometimes hits the network based on its argument is worse than
  one that never does.
- Invariant 10 says `secret set` never writes a value to disk. Reconcile
  resolves values into memory. Keeping the two apart keeps that invariant easy
  to audit: one function family touches refs, another resolves them.
- Invariant 6's spirit: a transient failure must not be turned into a hard
  failure of an unrelated command. If the Anthropic API is down, `secret set`
  should still succeed at writing a ref.

Same argument for `secret sync`, plus one more: `secret sync` is the documented
recovery path in `run`'s "you have refs, resolve them" arm
(`run.go:851`). It must stay fast and must not acquire a new failure mode.

**So the coverage is three layers, all naming the same command:**

1. The moment (`secret set` / `secret sync` print a grounded one-line nudge)
2. The standing check (`pix doctor`, below)
3. The noun (`pix models` bare status, above)

The nudge is **conditional and grounded**, never unconditional noise. It prints
only when all of: the key just written is one of the three provider keys; the
config has no callable binding for that provider; and
`cfg.Inference.ExclusiveSource == ""`.

## Roster widening

### The config field

New field on `config.InferenceConfig`, immediately after `AllowedModels`
(`services/host/config/config.go:240`):

```go
// RosterProviders records which providers were on the table the last time
// AllowedModels was chosen. It is the memory that lets a NEWLY added provider
// widen the roster automatically while an explicit narrowing WITHIN
// already-seen providers survives. Empty means "written before this field
// existed": reconstructed from the roster's own providers plus the providers
// bound BEFORE the current call mutated anything (never after — see
// configureModelRoster's `prior` parameter), so an upgrade re-run widens
// nothing while a genuinely new provider still widens.
RosterProviders []string `toml:"roster_providers,omitempty"`
```

Invariant 1 compliance: `omitempty` + the existing sparse encoder means the key
is absent from `config.toml` until it deviates from the zero value, so a future
default change still reaches users. It is written by `configureModelRoster` (a
setup/`models` mutation), not by hand — consistent with the rest of
`InferenceConfig`, which `config set` does not expose and users never edit.

### The algorithm

`configureModelRoster` gains one parameter:

```go
// prior is the set of providers that had native bindings BEFORE the caller
// mutated them on this pass. It MUST be captured before configureDirectInference
// runs; that function rebuilds every native binding from the catalog, so a
// `prior` read afterwards contains the provider the user is adding right now and
// the widening below degenerates to a no-op. On paths that bind nothing (the
// pack/gateway/ollama branch of setup) prior == the current bound set and the
// widening is a no-op by construction, which is the intent.
func configureModelRoster(cfg *config.Config, in io.Reader, out io.Writer,
	interactive bool, requested string, prior []string) error
```

Replace the preserve-and-prune block at `inference.go:289` with:

```go
boundProviders := providersOf(bound)   // POST-mutation: what is callable now
seen := set(cfg.Inference.RosterProviders)
if len(seen) == 0 {
	// Legacy config (field predates this change). Reconstruct what the user
	// could plausibly have been choosing over the last time the roster was
	// set: the providers their roster already names, plus whatever was bound
	// BEFORE this call. Never boundProviders — that is post-mutation and would
	// mark the provider being added right now as already seen.
	seen = set(union(providersOf(cfg.Inference.AllowedModels), prior))
}

var kept []string
for _, id := range cfg.Inference.AllowedModels {
	if bound[id] { kept = append(kept, id) }       // prune stale (unchanged)
}
if len(kept) > 0 {
	for _, m := range candidates {                  // widen for NEW providers only
		if !seen[m.Provider] && !contains(kept, m.ID) { kept = append(kept, m.ID) }
	}
	cfg.Inference.AllowedModels = kept
	cfg.Inference.RosterProviders = sorted(boundProviders)
	return nil
}
// kept empty (every previous choice went stale): fall through to the prompt,
// exactly as today.
```

The explicit `--models` path also stamps
`cfg.Inference.RosterProviders = sorted(boundProviders)`: the user just made a
fully explicit choice over the current provider set, so that set is "seen".

### The trace that revision 1 got wrong

This is the exact user the doc exists for, and it is worth walking because the
failure was silent. Existing install, `AllowedModels` = the Anthropic models,
no `roster_providers` key. They run `pix models add google`:

| step | revision 1 (broken) | revision 2 |
|---|---|---|
| `prior` | not captured | `{anthropic}` |
| `configureDirectInference` | adds the google backend + bindings | same |
| `seen` (legacy arm) | `providersOf(bound)` = **`{anthropic, google}`** — google is already bound by the previous step | `union(providersOf(AllowedModels), prior)` = `{anthropic}` |
| widen loop | skips google as "seen" → `AllowedModels` stays Anthropic-only | appends the google models |
| `RosterProviders` stamped | `[anthropic, google]` → google is **permanently** marked seen | `[anthropic, google]`, correctly, because it is now in the roster |
| `verifyDirectInference` | `inferenceBindingAllowed` rejects every google binding (not in the non-empty `AllowedModels`), so only Anthropic is probed | probes the google bindings too |
| output | `verified > 0` → **prints success** | success is earned, and a google probe failure surfaces |
| result | A3 reproduced verbatim, now behind a success message: no manifest entry, no `routing.json` entry, `resolveSessionModel` will not resolve it | the key works |

The two stated properties ("add a second provider → its models appear" and
"legacy → freeze as today") were mutually exclusive as specified, because
"today's bound set" was computed after the mutation. They are compatible once
"today" means pre-mutation.

Properties this gives you:

- **Legacy config + `pix models add google` → widens.** The flagship case.
  `prior` is `{anthropic}`, google is unseen, its models land in the roster and
  get probed. This is the one that revision 1 failed and revision 1's test 8
  wrongly enshrined as correct.
- **Legacy config + a plain `pix setup` re-run with no new key → widens
  nothing.** `prior` == the post set, `seen` covers everything callable, the
  widen loop finds no unseen provider. `RosterProviders` is stamped, which is
  the only visible change. No surprise behavior change on a version bump.
- **Legacy config + `pix setup` re-run when a key exists that was never wired**
  (literally the A3 state) → widens, because `prior` does not contain that
  provider. That is repair, not surprise: the user set the key on purpose and
  pix ignored it. It is the deliberate exception to "upgrades change nothing".
- Narrow to `anthropic/claude-sonnet-5` only, then re-run setup → still
  Sonnet only. The narrowing is within a seen provider; it is preserved.
- Narrow to Sonnet only, then add Google → Sonnet **plus** the Google models.
  This is the deliberate tradeoff: a new provider is a new decision, and
  "I bought a Gemini key and pix ignored it" is the failure we are fixing.
  A user who wants Sonnet-only-forever runs `pix models setup` (or
  `pix models add google --models anthropic/claude-sonnet-5`) once.
- Remove a provider → `boundProviders` shrinks, its models prune,
  `RosterProviders` shrinks. Re-adding later widens again. Correct and
  symmetric.

**Fitness function.** `prior`-before-mutation is a property no type system
enforces and one refactor can erase, so it gets a behavioral test that fails
loudly (test 7) rather than a comment alone. The test asserts the widened
roster *and* that the probe stub was asked for the new provider's models —
because it is the probe gating, not the roster field, that turns this bug into
a false success print.

### Alternatives evaluated

| option | verdict | why |
|---|---|---|
| `--models` flag on `models add` | **ship it, but not as the fix** | It is the right non-interactive escape hatch and mirrors `setup --models`. But it requires knowing catalog model ids by hand, which is exactly the knowledge the user does not have. It cannot be the primary answer. |
| interactive re-prompt on every add | reject | Re-asks a question already answered, and is impossible under `--yes`/CI (`setup --non-interactive` exists for a reason). Adding a key is not the moment to re-litigate the roster. |
| `models roster` subcommand | reject | A third way to do what `models setup` (interactive) and `--models` (explicit) already do. YAGNI on a surface whose whole complaint is that it has too many un-findable verbs. |
| widen unconditionally on every run | reject | Destroys a deliberate narrowing. `RosterProviders` exists precisely to tell "new provider" apart from "provider you already decided about". |

### Pack behavior (`ExclusiveSource`)

`configureModelRoster` already early-returns when
`cfg.Inference.ExclusiveSource != ""` (`inference.go:220`). Widening therefore
never runs under a mandatory pack. Explicitly:

- **`pix models add` under a mandatory pack refuses**, before any mutation,
  exit 2:

  ```
  pix models add: pack "git+https://github.com/acme/work-pack.git#ref=main"
  owns inference exclusively; personal provider keys are not used here.

  Storing the ref now is still fine — pix secret set writes op:// refs only,
  never a value, and nothing else changes. It stays unwired until a pack that
  allows personal keys is active.

  Then:  pix pack ls              see what is active
         pix pack use <other>     switch
         pix models add google    wire it in
  Or ask the pack owner to add the provider.
  ```

  The "stash the ref now" sentence exists because the common case is a
  *temporary* pack (a work pack active for the afternoon). Refusing without it
  reads as "you cannot do this at all", which sends the user looking for
  another dead end. It also states invariant 10 in passing, where it is
  actually reassuring.

  It refuses rather than half-succeeding because `configureDirectInference`
  would happily write a native backend that `inferenceBindingTopologyAllowed`
  and `inferenceBindingAllowed` then filter out at every read — a silent no-op
  that looks like success. Invariant 13 forbids printing a success word for
  that.
- **`reconcileDirectInference` returns `errInferenceExclusive` before touching
  `cfg`.** `models add` renders the message above; setup's keys step never
  reaches it (`setupChooseInference` short-circuits on a pack-supplied backend).
- **`AllowedModels` and `RosterProviders` are preserved, not deleted**, matching
  today's documented behavior ("exclusive pack inference bypasses this personal
  preference without deleting it, so switching back restores the personal
  roster", `config.go:236–239`).
- **`models`, `models ls|show|pick|route` all still work.** They read the
  registry/scorecard/policy, not personal bindings. Bare `pix models` shows the
  pack as the runtime and names it.

## Doctor check

New check appended in `providersGroup` (`doctor_providers.go:46`), gated the
same way `providerInfoCheck` is — on `inferenceNeedsOnePassword(cfg)`, i.e. the
direct-key topology. Gateway and Ollama hosts have no `hostmode.env` provider
key to be inconsistent with.

```go
// inferenceBindingGapCheck catches the specific dead end that made a correctly
// configured key inert: a provider key is present in hostmode.env, but no
// callable binding for that provider exists in cfg.Inference.Models, so no
// agent can ever route to it. It reads a local file and the config only — no
// probe, no network — so it makes no claim beyond "a binding exists". Whether
// that binding WORKS is Verified's job, earned by verifyDirectInference.
func inferenceBindingGapCheck(cfg *config.Config, env shellEnv) check
```

Verdict table, matching the existing structure exactly:

| condition | requirement | verdict | note | detail / evidence / todo |
|---|---|---|---|---|
| `hostModeProviderKeys` errors | optional | `verdictUnverifiable` | true | detail: `cannot read provider refs (<err>)`; evidence: `hostmode.env: unreadable` |
| no gap (every keyed provider has a callable binding) | optional | `verdictReady` | true | detail: `every configured provider key has model bindings`; evidence: `hostmode.env: anthropic, openai; inference backends: anthropic, openai` |
| gap, and `ExclusiveSource != ""` | optional | `verdictUnverifiable` | true | detail: `pack <source> owns inference; the google key is unused here`; evidence: `exclusive_source=<source>` |
| gap, no pack | **optional** | `verdictTodo` | **false** | detail: `google key is set but has no model bindings — agents cannot use it`; evidence: `hostmode.env: anthropic, google; inference backends: anthropic`; todo: `pix models add google` |

Two deliberate choices in that table:

- **`requirementOptional`, not core.** `inferenceCoreCheck` already owns launch
  readiness, and a second inert provider does not stop you launching. Per
  `TestExitMatrix`'s `"optional todo never blocks"` case
  (`verbcoverage_test.go:140`), this shows up in
  outstanding without changing `pix doctor`'s exit code. Visible, not blocking.
  The one case where the gap *is* launch-critical — the gap contains the
  `run_intent` provider — is already owned by `runIntentKeyCheck`
  (`doctor_providers.go:65`); don't double up the todo. That deferral is only
  safe **because copy change 12 fixes that check's todo**: today it hands out
  `pix secret set <P>_API_KEY … && pix secret sync`, which lands the user in
  the exact state this new check exists to report. Ship the two together.
- **`note: false` on the real gap.** `note: true` excludes a check from
  `outstanding()` (`readiness_types.go:273`) and `blockingCheck`
  (`readiness_types.go:184`) regardless of verdict — the field's own doc
  comment at `readiness_types.go:101` says so. A note is
  exactly what the current behavior effectively is: invisible. The whole point
  is to make it counted.

Invariant 13: the `ready` arm's detail says "has model bindings", never
"verified" or "working". That is precisely what a config read proves.

## Copy changes

Every user-facing string, old → new. Verbatim. Revision 1 claimed this list was
complete and was not: items 12–17 were missing, and four of them
(`route.go:41`, `agent.go:333/358/395`) would have been flagged by this doc's
own proposed copy guard on the first CI run. The inventory below was
re-derived by grepping `pix route` and `pix secret set` across
`services/host/cmd/pix/*.go` (excluding `_test.go`), so it is the whole set as
of this revision — and the two guards in the test plan are what keep it whole.

### 1. `setup.go:1391` `promptProviderChoice`

Old:
```go
fmt.Fprintln(out, "One model provider is enough to start. You can add others later.")
```

New:
```go
fmt.Fprintln(out, "One model provider is enough to start.")
fmt.Fprintln(out, "Add another whenever you want:  pix models add openai")
fmt.Fprintln(out, "(or anthropic, or google — it asks for that provider's 1Password ref,")
fmt.Fprintln(out, "wires its models in, and checks they answer.)")
```

Rendered:
```
One model provider is enough to start.
Add another whenever you want:  pix models add openai
(or anthropic, or google — it asks for that provider's 1Password ref,
wires its models in, and checks they answer.)
  1. openai (default)
  2. anthropic
  3. google
Choose a provider [1]:
```

### 2. `doctor_providers.go:118` `modelKeyFixCmd`

Old:
```go
const modelKeyFixCmd = "pix secret set ANTHROPIC_API_KEY op://vault/item/field && pix secret sync"
```

New:
```go
const modelKeyFixCmd = "pix models add anthropic"
```

`pix models add <provider>` is a strict superset of the old two-command
sequence: it prompts for and validates the ref (`promptProviderRef`), writes
`op-refs.env` + `hostmode.env`, reconciles into sbx
(`reconcileProviderKeysWithSbx`), derives bindings, widens the roster, and
probes. The comment above the const keeps its meaning — one provider named as
the example, the other two in the core check's evidence.

### 3. `run.go:851` `modelKeyMissingMessage`, the "no refs at all" arm

Old:
```go
msg += "Keys come from 1Password (op is required). Configure them, then re-run:\n" +
	"  pix setup                                                       (guided, all providers)\n" +
	"  pix secret set ANTHROPIC_API_KEY op://vault/item/field && pix secret sync   (one provider)\n"
```

New:
```go
msg += "Keys come from 1Password (op is required). Configure them, then re-run:\n" +
	"  pix setup                  (guided: runtime, keys, models, in one pass)\n" +
	"  pix models add anthropic   (one provider: ref, sbx, bindings, live check)\n"
```

The `providerKeyRefsPresent` arm above it is unchanged — `pix secret sync` is
still the right answer when the refs exist and only sbx is behind. Invariant 6
is untouched: this is how-to-fix text, not the tri-state launch gate.

### 4. `secret_sync.go:748` `runSecretSync`, the fatal arm

Old:
```go
fmt.Fprintln(out, "Add provider keys with: pix secret set ANTHROPIC_API_KEY op://vault/item/field")
```

New:
```go
fmt.Fprintln(out, "Add a provider key with:  pix models add anthropic")
fmt.Fprintln(out, "(refs only, no values on disk — pix reads them from 1Password at use.)")
```

### 5. NEW — `runSecretSync` success arm, grounded nudge

Printed after the `%d provider key(s) synced` line, only when at least one
synced provider has no callable binding and `ExclusiveSource == ""`:

```
  ! google has a key but no model bindings yet — agents cannot use it.
    Wire it in:  pix models add google
```

### 6. NEW — `runSecretSet` grounded nudge

Printed after a successful `secret set` of `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, or `GEMINI_API_KEY`, only when that provider has no callable
binding and `ExclusiveSource == ""`:

```
  ref saved. google is not wired into inference yet — finish with:
    pix models add google
```

Nothing is printed for a non-provider key, or when the binding already exists.

### 7. `help.go:143` — the help listing

Old:
```
Models & agents (cost/latency/accuracy routing)
  agent <cmd>         ls | new | edit | rm | reassess (subagents as objects)
  route <cmd>         pick | compile | show | models (intent -> model)
```

New:
```
Models & agents (cost/latency/accuracy routing)
  models              which models pix can use, and which are wired up
  models add <prov>   add a provider key (anthropic | openai | google)
  models route        recompile the intent -> model map the sandbox reads
  agent <cmd>         ls | new | edit | rm | reassess (subagents as objects)
```

`ls | show | pick` live in `pix help models` / `modelsUsage()`, one level down.
Progressive disclosure (CLI-redesign taste rule #2).

### 8. `main.go:319` — the `--intent` flag help

Old: `--model overrides it. Intents: pix route show`
New: `--model overrides it. Intents: pix models show`

### 9. `main.go:117` — the retired `evals` verb message

Old: `fmt.Fprintf(os.Stderr, "  %s; run \`pix route compile\`\n", routing.ScorecardPath())`
New: `fmt.Fprintf(os.Stderr, "  %s; run \`pix models route\`\n", routing.ScorecardPath())`

### 10. `route.go` `routeUsage()` → `modelsUsage()`

Retitled `usage: pix models <command>`, with `add` and `setup` added and
`route` described as "resolve every intent and write routing.json". `route` and
`add`/`setup` carry an explicit `(writes)` marker that `ls`, `show` and `pick`
do not — mitigation (b) for the read-only/mutating asymmetry flagged under
The verb tree:

```
  ls                  list the model registry
  show                registry + scorecard + the resolved intent table
  pick <intent>       resolve one intent, with the rationale
  route               resolve every intent and write routing.json   (writes)
  add [provider...]   wire a provider key into inference               (writes)
  setup               re-ask only the inference question              (writes)
```

The existing
paragraph about `routing.ModelsPath()` / `routing.ScorecardPath()` being the
real resolved override paths is preserved verbatim — that comment
("so the override paths it prints are the REAL resolved paths … never the
repo's embedded default source") is load-bearing and must survive the rename.

### 11. NEW — `pix route` deprecation line (stderr, one release)

```
pix route is now pix models (pix models route compiles the intent map).
```

stderr only, so `--json` and piped stdout are unaffected (test 21). Drop this
string entirely if the hard cut is taken.

### 12. `doctor_providers.go:112` — `runIntentKeyCheck`'s todo

This is the SHOULD-FIX the reviewer was most right about: the doc argued
correctly that `secret set` must not auto-reconcile, then left the one check it
calls "already owned" still handing out the dead-end command.

Old:
```go
todo: "pix secret set " + strings.ToUpper(provider) + "_API_KEY op://vault/item/field && pix secret sync"
```

New:
```go
todo: "pix models add " + provider
```

Following the old todo produces a key with no bindings — exactly A3 — in the
one case where the gap *is* launch-critical (the missing provider is
`run_intent`'s). The detail line above it is unchanged; only the copy-pasteable
command moves. This is also what makes the "don't double up the todo" decision
in the doctor section safe: `inferenceBindingGapCheck` and `runIntentKeyCheck`
now name the same command, so whichever one fires, the user types the same
thing.

### 13. `setup.go:1089` — setup's `provider keys` core check

Old:
```go
todo: "pix secret set ANTHROPIC_API_KEY op://Vault/Item/field"
```

New:
```go
todo: "pix models add anthropic"
```

Same reasoning as item 2. This one fires on a host with zero provider keys, so
the old command leaves the user one step short every time.

### 14. `setup.go:1276` — the non-interactive "no provider configured" block

Old:
```go
fmt.Fprintln(out, "No model provider is configured. Add any ONE provider:")
for _, p := range providerKeyRefOrder {
	fmt.Fprintf(out, "  pix secret set %s op://Vault/Item/field  # %s\n", p.envVar, p.name)
}
fmt.Fprintln(out, "then re-run: pix setup")
```

New:
```go
fmt.Fprintln(out, "No model provider is configured. Add any ONE provider:")
fmt.Fprintln(out, "  pix models add anthropic | openai | google   (prompts for the op:// ref, wires it in)")
fmt.Fprintln(out, "Or store the refs first, then re-run setup:")
for _, p := range providerKeyRefOrder {
	fmt.Fprintf(out, "  pix secret set %s op://Vault/Item/field  # %s\n", p.envVar, p.name)
}
fmt.Fprintln(out, "then re-run: pix setup")
```

**Partially argued down.** Unlike items 12 and 13, this block is not a dead end
today: it already ends with `then re-run: pix setup`, which does reach
`configureDirectInference`. So the `secret set` lines stay — they are the
right mechanism for a scripted host that wants to place all three refs before
any reconcile. What changes is that the one-command path is named **first**, so
the fast answer is not buried under three lines of the slow one.

`setup.go:1295` (`fix it: pix secret set %s op://Vault/Item/field`, printed
when a ref fails to resolve mid-setup) is **left alone**: setup continues to
`configureDirectInference` on that path, so fixing the ref is genuinely the
whole fix. Naming `pix models add` there would tell the user to restart a flow
they are already inside.

### 15. `route.go:41` — `resolveSessionModel`'s unknown-intent error

Old:
```go
return "", fmt.Errorf("unknown intent %q (see `pix route show` for the intent list)", intent)
```

New:
```go
return "", fmt.Errorf("unknown intent %q (see `pix models show` for the intent list)", intent)
```

`resolveSessionModel` itself does not move (blast radius table), but its copy
still has to follow the rename. This is one of the four strings the proposed
guard would have failed on.

### 16. `agent.go:333`, `:358`, `:395` — the agent-authoring guidance

Three strings, all in the `agent` verb, all naming the old spelling:

| line | old | new |
|---|---|---|
| `agent.go:333` | ``"`route show`). Tune the tradeoffs in policy.json, then `pix route compile`."`` | ``"`pix models show`). Tune the tradeoffs in policy.json, then `pix models route`."`` |
| `agent.go:358` | `"… will inherit the parent model until you add it (pix route show).\n"` | `"… will inherit the parent model until you add it (pix models show).\n"` |
| `agent.go:395` | `"  3. pix route compile                     # route it\n"` | `"  3. pix models route                      # route it\n"` |

Note `:333` carries a bare `` `route show` `` as well as `pix route compile`;
both halves change, which is why the row shows the whole fragment. `agent` is
the sibling verb in the same help group as `models`, so leaving it on the old
noun would be the most visible possible inconsistency.

### 17. `route.go:1` — the file header comment

Old: `// pix route — a thin launcher passthrough to the sibling pix-host`
New: `// pix models — a thin launcher passthrough to the sibling pix-host`

Not user-facing, but the file is renamed to `models.go` and the source guard
reads comments too.

## Rename blast radius

| file | change |
|---|---|
| `services/host/cmd/pix/route.go` | rename to `models.go`. `runRoute` → `runModels` with the launcher-side subcommand switch (`ls\|show\|pick\|route\|compile\|add\|setup\|""`), mapping to `execHost("route", …)` for the four passthroughs. `routeUsage()` → `modelsUsage()` (copy change 10), with `route` annotated `(writes routing.json)`. `resolveSessionModel` and `execHost` keep their behavior and stay here, but **`resolveSessionModel`'s error string at `:41` changes** (copy change 15), as does the file header comment at `:1` (copy change 17). |
| `services/host/cmd/pix/models_add.go` | **new.** `runModelsAdd`, `runModelsSetup`, flag parsing, the pack refusal. |
| `services/host/cmd/pix/inference.go` | **new** `reconcileDirectInference` + `reconcileOpts`/`reconcileResult` + `boundNativeProviders`; `configureModelRoster` gains the `prior` parameter and the widening block replaces `:289`. |
| `services/host/cmd/pix/setup.go` | keys step's **tail only** (`:692–716`) calls the seam, head unchanged; both `configureModelRoster` call sites gain the `prior` argument (including the `selected` branch at `:678`); `promptProviderChoice` copy (`:1391`); the `provider keys` todo at `:1089`; the non-interactive block at `:1276`. `:1295` deliberately unchanged. |
| `services/host/cmd/pix/main.go` | dispatch `case "models": runModels(args[1:])`; keep `case "route":` printing the deprecation to stderr then calling `runModels`. Lines 117 and 319 copy. |
| `services/host/cmd/pix/help.go` | `knownVerbs`: add `"models"`, **remove** `"route"`. `retiredVerbs`: add `"route": "models"`. Line ~143 listing. Line ~234 `verbUsage` case `"route"` → `"models"` → `modelsUsage()`. |
| `services/host/cmd/pix/man.go` | any `route` reference → `models`. |
| `services/host/cmd/pix/pix.1` | delete `.SS route` (lines 714–740); add `.SS models` with `.BR "pix models"`, `… ls`, `… show`, `… pick`, `… route`, `… add`, `… setup` synopsis lines. Edit as ONE unit with `knownVerbs` — `man_test.go` enforces strict 1:1. |
| `services/host/cmd/pix/doctor_providers.go` | `modelKeyFixCmd` (`:118`); **`runIntentKeyCheck`'s todo at `:112`** (copy change 12); new `inferenceBindingGapCheck`; append it in `providersGroup`. |
| `services/host/cmd/pix/agent.go` | three guidance strings at `:333`, `:358`, `:395` (copy change 16). Absent from revision 1's table; the new copy guard would have failed on all three. |
| `services/host/cmd/pix/run.go` | `modelKeyMissingMessage` copy. |
| `services/host/cmd/pix/secret.go` | grounded post-set nudge. |
| `services/host/cmd/pix/secret_sync.go` | fatal-arm copy + grounded post-sync nudge. |
| `services/host/config/config.go` | `InferenceConfig.RosterProviders`. |
| `services/host/cmd/pix/verbcoverage_test.go` | `hiddenVerbs["route"] = "deprecated alias of models; removed after one release"`. Add `"models": {"ls","show","pick","route","add","setup"}` to `TestEveryDispatchedSubcommandAppearsInItsUsage`'s table. |
| `services/host/cmd/pix/copy_guard_test.go` | **two new guards**, same shape as `TestNoRawGogAuthLoginInProductionSource` (`copy_guard_test.go:30`): (a) ban `\bpix route\b` in production `.go` source — `pix-host route` does not contain that substring, so no allowlist is needed; (b) ban a literal `pix secret set (ANTHROPIC\|OPENAI\|GEMINI)_API_KEY`. Guard (b) has a stated blind spot; see the test plan. |
| `Makefile` | `route:` target → `models:` (`.PHONY`, the `@echo` at line 128, the maintainer comment at 267–276). It still invokes `./out/pix-host route`, which does not move. Keep `route:` as a `.PHONY` alias that prints a one-liner and delegates. |
| `extensions/subagents.ts` | line ~300 error text `pix route compile` → `pix models route`. Requires `make load` to reach a baked image; the host rename works without it. |
| `docs/reference.md` | the `| route |` capability-map row → `| models |`; the `pix route pick <intent>` example at line ~174. |
| `docs/design/routing.md` | every `pix route <sub>` → `pix models <sub>`; add a pointer to this doc. |
| `docs/design/onboarding-v3.md` | the "route compiler" mention; the later-path sentence. |
| `docs/README.md` | index entry. |
| `skills/model-refresh/SKILL.md` | frontmatter description (`route show`), lines 20, 100, 101, 116. |
| `AGENTS.md` | the `routing.json` repo-layout row, the `pix-host` row (`route` stays as the host subcommand — say so), the Models & subagents bullet (`pix route compile` → `pix models route`), and a new sentence: adding a provider later is `pix models add <provider>`. |
| `CHANGELOG.md` | one entry covering both fixes and the deprecation window. |

### Deprecation: alias for one release as a courtesy, not a constraint

**Decision: keep `case "route"` dispatching for exactly one release, printing a
one-line stderr deprecation and continuing. Then delete the case and rely on
`retiredVerbs`.** This is a **low-stakes, reversible call**, and revision 1
oversold it. Two claims are withdrawn.

**Withdrawn: "the tests force a `hiddenVerbs` entry."** They do not.
`TestHelpListsEveryTopLevelVerb` (`verbcoverage_test.go:~87`) does naive
substring matching — `strings.Contains(helpAllText, verb)` — not a
word-boundary or line-shape match. The proposed help listing (copy change 7)
contains the literal token `route` in its `models route` line, so a surviving
`case "route"` passes with **no** `hiddenVerbs` entry at all. The entry is
still worth adding: `hiddenVerbs`'s contract is "deliberately absent from the
help tree", `route` genuinely is absent as a *verb*, and the required reason
string is a good place to record the removal release. But that is hygiene the
implementer chooses, not a constraint the build imposes, and "it forces the
removal date into the source" was wishful — nothing enforces the date.

**Withdrawn: the image-skew argument.** `extensions/subagents.ts` never shells
out to `pix`. It reads `routing.json` once, offline
(`subagents.ts:127`, `:147`); the string `pix route compile` appears only in an
advisory warning (`subagents.ts:300`) emitted in the rare unknown-intent case.
So the worst a stale image can do is print a command that is one bounce from
working: under a hard cut the user types it, gets exit 2, and
`retiredVerbs["route"] = "models"` names the replacement. Revision 1 also
contradicted itself — the Migration section calls this same string "cosmetic"
while this section made it load-bearing. Migration was right.

**What actually justifies the alias:** it is one dispatch case plus one stderr
line, it protects shell history and any script the owner wrote during the
release when the old spelling is freshest, and it costs nothing to delete
later. That is a courtesy, and it is enough.

**The hard cut is equally defensible — take it if you prefer.** Delete
`case "route"`, keep `retiredVerbs["route"] = "models"` **permanently**, drop
the `hiddenVerbs` entry, drop `TestRouteAliasStillDispatches`. Recovery is one bounce; this is a
personal harness with one user; nothing external depends on the spelling. The
implementer should not spend more than a minute on this choice.

What is **not** optional either way:

- `retiredVerbs` (`help.go:49`) gets `"route": "models"` and keeps it forever.
  It already exists for exactly this (`onboard` → `setup --no-agent`, `gog` →
  `gworkspace`), and `suggestVerb`'s edit distance would never get from `route`
  to `models` on its own.
- `man_test.go` enforces strict 1:1 between `knownVerbs` and the `pix <verb>`
  synopsis lines in `pix.1` (`man_test.go:37–64` vs `pix.1:714–740`). `"route"`
  leaves `knownVerbs` and `.SS route` leaves `pix.1` in the **same commit**,
  under either option. A dispatch case that survives in the shell but is absent
  from `knownVerbs` and the man page is exactly the intended end state.

## Test plan

Every test below is named with the mutation it catches. Run from the module
root: `cd services/host && go test ./...`.

**Reconcile seam**

1. `TestReconcileDirectInferenceBindsNewProvider` — refs for anthropic+google
   in `hostmode.env`, config bound to anthropic only. Asserts
   `cfg.Inference.Backends["google"]` exists with `Driver:"native"`,
   `KeyEnv:"GEMINI_API_KEY"`, and that `cfg.Inference.Models` gains the google
   catalog models. *Catches: the entire Problem A dead end returning.*
2. `TestReconcileSavesBeforeVerifyFailure` — probe stub returns an error for
   every candidate. Asserts the function returns a non-nil error AND the saved
   `config.toml` on disk contains the new bindings. *Catches: someone "tidying"
   the save to after the verdict and throwing away durable progress on a
   network blip.*
3. `TestReconcilePartialVerifyIsNotAnError` — 3 of 4 probes succeed. Asserts
   `err == nil`, `Verified == 3`, `len(Failures) == 1`. *Catches: an
   entitlement gap on one model failing the whole command.*
4. `TestReconcileNoVerifyMakesNoVerifiedClaim` — `verify:false`. Asserts no
   binding has `Verified: true` and the rendered output contains neither
   "verified" nor "ready". *Invariant 13.*
5. `TestSetupKeysStepDelegatesToReconcile` — a package-level stub for the seam
   records one call with `verify:true`; asserts setup's keys step made exactly
   that call. *Catches: setup and `models add` drifting into two
   implementations, which is the root cause of this whole doc.*
6. `TestReconcileRefusesUnderExclusivePack` — `ExclusiveSource` set. Asserts
   `errInferenceExclusive`, and that `cfg.Inference.Backends`,
   `.Models`, and `.AllowedModels` are byte-identical before and after.
   *Catches: a half-mutation under a mandatory pack.*

**Roster widening** — revision 1's tests 7 and 8 are **replaced**, not amended.
Between them they asserted the bug as correct: old test 8 pinned "legacy config
does not widen" (which, with the post-mutation seen-set, meant `models add`
never widened either), and old test 7 pre-set `RosterProviders: ["anthropic"]`,
so **no test exercised the legacy `models add` path** — the only path that
exists on a real install today. The new 7 is that path, and it is the most
important test in this document.

7. `TestRosterLegacyAddWidensForTheNewProvider` — **the flagship.** Start from
   a config as it exists on a real machine today: `AllowedModels` = the
   Anthropic models, `RosterProviders` **absent**, backends/bindings anthropic
   only. Add a google ref to `hostmode.env`, run `reconcileDirectInference`
   with a probe stub that succeeds for every candidate. Asserts, in order of
   what each one catches:
   - `AllowedModels` now contains the google catalog models (the widening
     itself);
   - the probe stub **was asked for at least one `google/*` model** (the
     consequence that matters: `verifyDirectInference` only probes bindings
     that pass `inferenceBindingAllowed`, so a roster that did not widen makes
     the success print a lie);
   - `RosterProviders == [anthropic, google]`;
   - the returned `Added == ["google"]`.
   *Catches: computing the seen-set after `configureDirectInference` — i.e.
   silently reproducing the A3 dead end behind a success message, which is
   exactly what revision 1 specified.*
8. `TestRosterLegacySetupRerunWidensNothing` — same legacy config, but
   `hostmode.env` is **unchanged** (anthropic only). Run the seam. Asserts
   `AllowedModels` is byte-identical and the only change is
   `RosterProviders` being stamped to `[anthropic]`. *Catches: a version bump
   silently re-widening a deliberate narrowing on the next `pix setup`. This is
   the property old test 8 was reaching for; it is preserved, but it is now
   pinned by "no new provider", not by "legacy config".*
9. `TestRosterExplicitNarrowingSurvivesNewProvider` — `AllowedModels` narrowed
   to one Anthropic model with `RosterProviders: ["anthropic"]` already
   stamped, then google binds. Asserts the single-model Anthropic narrowing
   survives AND google's models are appended. *Catches: a widen that destroys a
   deliberate within-provider choice. (This is old test 7, kept for its own
   sake — it is a real property, it was just never the flagship.)*
10. `TestRosterRepairsUnwiredKeyOnSetupRerun` — the A3 state itself: legacy
    config, google key present in `hostmode.env`, no google bindings. Run
    `pix setup`'s path. Asserts the roster widens to include google. *Pins the
    deliberate exception to "an upgrade changes nothing": a key the user set on
    purpose and pix ignored gets repaired, not frozen in place.*
11. `TestRosterProvidersOmittedWhenUnset` — a config that never set the field
    round-trips through `Save()` with no `roster_providers` key in the file.
    *Invariant 1: `Save()` persists only explicit deviations.*
12. `TestRosterPrunesStaleAfterProviderRemoval` — google ref removed. Asserts
    google models leave `AllowedModels` and `RosterProviders`.

**Doctor**

13. `TestInferenceBindingGapCheckTodo` — key present, no binding. Asserts
    `verdict == verdictTodo`, `note == false`, `requirement == requirementOptional`,
    `todo == "pix models add google"`, and that the evidence names both sides.
    *Catches: the gap being invisible, and it being made a blocking core check.*
14. `TestInferenceBindingGapCheckDoesNotBlockExit` — a snapshot containing a
    ready core check plus this todo. Asserts `ExitCode() == exitReady`.
    *Catches: `pix doctor` starting to exit 1 for a non-blocking condition.*
15. `TestInferenceBindingGapUnderPackIsNotATodo` — `ExclusiveSource` set.
    Asserts `verdict == verdictUnverifiable`, `note == true`, empty `todo`.
    *Catches: telling a pack user to run a command that will refuse.*
16. `TestInferenceBindingGapMakesNoVerifiedClaim` — the no-gap arm's detail must
    not contain "verified" or "working". *Invariant 13.*

**Rename / discoverability**

17. `TestHelpListsEveryTopLevelVerb` (existing) — passes once `models` appears
    in `helpAllText`. Note what this does **not** prove: the check is
    `strings.Contains(helpAllText, verb)`, and the help text contains the token
    `route` inside the `models route` line, so it would pass with or without a
    `hiddenVerbs` entry for a surviving `route` case. Do not cite this test as
    the reason for that entry (revision 1 did).
18. `TestEveryDispatchedSubcommandAppearsInItsUsage` (existing, extended) —
    `models` → `{ls, show, pick, route, add, setup}` must all appear in
    `modelsUsage()`. *Catches: `models add` shipping undiscoverable — the exact
    class of bug this doc exists to fix.*
19. `TestManPageMatchesKnownVerbs` (existing `man_test.go`) — fails unless
    `pix.1` gains `.SS models` and loses `.SS route` in the same commit.
20. `TestNoRawPixRouteInProductionSource` — **new** copy guard, modeled on
    `TestNoRawGogAuthLoginInProductionSource`. Bans `pix route` in production
    `.go` files. `pix-host route` does not contain that substring, so it needs
    no allowlist. *Catches: the old spelling regressing into a help string or a
    doctor todo.* Note this guard is what would have caught revision 1's four
    missed strings (`route.go:41`, `agent.go:333/358/395`) — write the guard
    and the copy changes in the same commit and let it find them.
21. `TestRouteAliasStillDispatches` — `route show` reaches the same handler as
    `models show` and prints the deprecation line to stderr, not stdout.
    *Catches: the deprecation line corrupting `--json` or piped output.*
    Delete this one along with the alias if the hard cut is taken.

**Copy**

22. `TestProviderChoicePromptNamesTheAddCommand` — asserts the rendered prompt
    contains `pix models add`. *Catches: the literal complaint. This is the
    single highest-value assertion in the list.*
23. `TestSecretSetNudgeIsGrounded` — three cases: provider key with a gap
    (nudge printed), provider key already bound (silent), non-provider key
    (silent). *Catches: unconditional nagging, which trains users to ignore it.*
24. `TestSecretSetDoesNotProbe` — a probe stub that fails the test if called.
    Asserts `secret set` and `secret sync` never invoke
    `env.directInferenceProbe`. *Catches: someone "helpfully" auto-reconciling
    and turning a fast offline file write into a networked command.*
25. `TestModelKeyGuidanceNamesModelsAdd` — **new**, rendered output, three
    cases: `runIntentKeyCheck` with a key for a non-`run_intent` provider
    (`doctor_providers.go:112`), setup's zero-key `provider keys` check
    (`setup.go:1089`), and setup's non-interactive no-provider block
    (`setup.go:1276`). Each must contain `pix models add`; the first two must
    **not** contain `pix secret set`. *Catches the SHOULD-FIX the reviewer
    found: the dead end surviving in a guidance string the rename did not
    touch.* This test, not the source guard, is the real coverage for
    `runIntentKeyCheck`, because that todo is **built by concatenation**
    (`"pix secret set " + strings.ToUpper(provider) + "_API_KEY …"`) and
    therefore is not a source literal any regex over `.go` text can see.
26. `TestNoRawProviderKeySecretSetInProductionSource` — **new** source guard
    banning a literal `pix secret set (ANTHROPIC|OPENAI|GEMINI)_API_KEY` in
    production `.go` files. Deliberately narrow, and the narrowness is the
    design:
    - It targets the exact shape that creates a key with no bindings. It does
      **not** ban `pix secret set` generally — that is the correct mechanism
      for `SLACK_TOKEN`, `GOG_*`, and pack credentials, and for the
      `%s`-formatted lines at `setup.go:1276` and `:1295` where setup itself
      does the wiring afterwards. Those `%s` forms do not match the pattern,
      which is why they can stay.
    - Rejected: an "adjacent `pix models add` within N lines" heuristic. It is
      unpredictable, it invites gaming by moving a line, and a guard whose
      verdict depends on formatting is a guard people delete.
    - **Stated blind spot:** a concatenated key name (the `runIntentKeyCheck`
      form) is invisible to it. Test 25 covers that case at the rendered-output
      level. Two mechanisms, one property; say so in the guard's doc comment so
      the next reader does not assume the source guard is complete.

## Migration and compatibility

- **Config.** `roster_providers` is additive and `omitempty`. An older `pix`
  reading a newer `config.toml` treats it as an unknown key, which
  `partitionUndecoded` tolerates and reports via `UnknownKeys()` — no hard
  failure (`config.go`'s documented contract). No migration step, no `pix state
  backup` required.
- **Behavior on upgrade.** Zero change until the user runs `pix models add`,
  `pix models setup`, or `pix setup`. `RosterProviders` is stamped on the first
  of those. The stamping itself widens nothing **provided the seen-set is
  reconstructed from the PRE-mutation bound providers** — that is the whole
  content of BLOCKER 1, and it is the difference between "nothing changes on
  upgrade" and "nothing ever changes, including the thing you asked for". The
  one intended exception: a provider key that was already on disk but never
  wired (the A3 state) gets picked up on the next run, because it was never in
  the pre-mutation bound set. That is repair, and it is tested (test 10).
- **`routing.json` and the image.** Unchanged format, unchanged compile path
  (`pix-host route compile`). No `make load` needed for the host-side rename.
  The only baked artifact touched is `extensions/subagents.ts`'s advisory
  warning string (`subagents.ts:300`), which is cosmetic — the extension reads
  `routing.json` offline (`:127`, `:147`) and never shells out to `pix` — and
  rolls out with the next image. The Deprecation section agrees with this now;
  in revision 1 it did not.
- **Scripts and CI.** `pix route <sub>` keeps working for one release with a
  stderr-only deprecation, so a script parsing stdout or `--json` is unaffected.
  After removal, `retiredVerbs` prints `route` → `models` on exit 2.
- **`pix-host route`** is untouched. Maintainer muscle memory and the Makefile
  keep working.
- **Invariant 5** is preserved end to end: `models add` on the direct-key path
  requires `op` and fails hard without it (it reuses `ensureSetupPrereqsFor` and
  `promptProviderRef`); on a pack/gateway/Ollama host it refuses or no-ops
  without ever mentioning 1Password.

## Open questions

1. **Should `pix models add` with no provider argument prompt, or stay silent?**
   Proposed: reconcile silently and report what changed
   (`wired in: google (3 models, 3 verified)`), so it is the idempotent
   "take my current keys into account" command. The prompting behavior is
   reserved for a named provider. Alternative: prompt for any provider that has
   no ref. Leaning silent — it makes the command safe in a script.
2. **Does `pix models add` recompile `routing.json` automatically?** A new
   provider changes what every intent can resolve to, so arguably yes. Against:
   `routing.json` in a dev checkout is written with `--out ./routing.json` and
   then baked, so an implicit compile could write the wrong file. Proposed:
   do **not** auto-compile; print `Next: pix models route` when the bound
   provider set changed. Wants a decision before implementation.
3. **Should `pix models` bare status be folded into `pix status`?** The
   inference block would be four more lines on the bare `pix` screen. Proposed:
   no — `pix status` stays a one-screen summary and links to `pix models`. But
   the one-line gap warning ("google is set but has no bindings") is short
   enough to earn a place on `pix status` too.
4. **Alias for one release, or hard cut now?** Revision 1 justified the alias
   with an image-skew argument that turned out to be false (see Deprecation).
   With that gone, both options are cheap: the alias is ~5 lines plus one test,
   the hard cut is zero lines plus a one-bounce `retiredVerbs` recovery.
   Proposed: alias for one release, because it is reversible and the owner's
   shell history is the only thing at stake. Implementer's call; do not spend
   time on it.
5. **`--no-verify` naming.** `--offline` may read better and would also skip the
   `op read` calls. Deferred until someone actually needs the offline path.
