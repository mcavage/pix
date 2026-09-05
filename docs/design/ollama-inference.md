# Ollama inference — local ladder, cloud entitlement, and a probe that earns "verified"

> **HISTORICAL (Wave F).** This document was written while pix had a scored
> model router, and it still describes one: scorecard rows, policy task types,
> `CompiledRouting`, `routing.json`, the `run_intent` doctor row. All of that
> was DELETED. Read it for the Ollama/local-hardware reasoning (the RAM
> formula, the rung ladder, the classification cascade), which survives in
> `services/host/inference/hardware.go`; treat every routing sentence as a
> record of what pix used to do.

**Status:** design. Nothing here is implemented. This doc replaces the current
one-size Ollama setup path with two explicit flows (local, cloud), a hardware
probe that sizes the local flow to the machine, and an Ollama verification probe
that mirrors `verifyDirectInference`.

Owner surfaces: `services/host/cmd/pix/{inference,setup}.go`, a new
`services/host/cmd/pix/readiness_hardware.go`, `services/host/routing/`. All
host Go (HOST = Go, SANDBOX = TypeScript).

## Review findings folded in

A cross-vendor adversarial review returned BLOCK on rev 1. What changed, and
what was argued down rather than changed:

| id | finding | resolution |
| --- | --- | --- |
| **B1** | concurrent probes co-load local weights → swap, or serialize behind each other → false timeout → an unverified *good* model gets un-bound | **fixed.** Local probes are **serialized** (concurrency retained only across the cloud set), each carries `keep_alive: 0` so its weights unload before the next starts, they run **largest rung first**, and the local set has its own wall budget. A candidate the budget never reached is `not probed`, a third state that is neither verified nor failed. D4. |
| **B2** | the migration doctor TODO is unimplementable — a listing-set `verified=true` is bit-identical to a probe-earned one | **fixed.** `InferenceModelBinding` gains optional `verified_by` (decision-bearing) and `verified_at` (evidence text only). Absent + `verified=true` ⇒ legacy claim. Data model section, migration section. |
| **S1** | `min_ram_gb` had no KV-cache term while the catalog advertised 256K context | **fixed.** `min_ram_gb = ceil(weights×1.15 + ctx_budget×kv_gb_per_token + 1)` with a **declared per-rung `ctx_budget_tokens`**, which is also the rung's `context_window` in the catalog and the `num_ctx` the probe and the bridge send. `darwin` fraction is now two-tier (0.67 / 0.75) against the macOS wired-memory limit. The whole RAM→rung table moved down a rung; a 32 GB Mac now gets the **9b**, not the 27b. D2. |
| **S2** | the new step's zero-verified check contradicted D5's exit-0 decline | **fixed.** The `inference` step is `fatal: false`, and — because `runSetupMutations` propagates a non-fatal error too (`setup.go:646-656`) — the decline path **returns `nil`**, not an error. The branch is spelled out on `models.consent`. D5. |
| **S3** | claim-6's caller trace missed `doctor_providers.go:126,155`, and doctor mis-remediates the new state with an `ANTHROPIC_API_KEY` fix | **fixed.** New `unverifiedOllamaCandidates` branch in `inferenceCoreCheck` and in the `run_intent` row, remediating with `pix setup --pull-models`. D4 → *Doctor*. |
| **S4** | "existing answers do not change meaning" is false for token `2`: it silently narrows to local-only | **owned, not fixed** — the narrowing *is* the fix. Called out explicitly in the migration section, with the `2,4` restore. |
| **S5** | `run`'s refusal carries no fix command, and invariant **6**'s mechanism is the sbx-secret key probe, not this | **fixed.** `synthesizeInferenceKit`'s error (`inference.go:673`) gains the remediation; the invariant-6 citation is corrected to a *spirit* argument with the actual gate named. D5. |
| **N1** | `--models X` non-interactive now errors when X failed its probe | mentioned and kept (D4 → *Roster*). |
| **N2** | `setupMutationOrder`'s comment + AC-P0-303, and an AC-P0-302 risk in the new step's output | fixed in D4 → *Ordering*, with tests. |
| **N3** | the pull prompt still says "optional", and the rung is persisted before consent | prompt header is now role-derived; the persist-before-consent ordering is **argued down** (the models step reads the tag from config, so it cannot be written after) with the doctor row as the named safety net. D5. |
| **N4** | `CompiledRoutingVersion` must NOT be bumped for `Relaxed` | restated as a loud, boxed rule in the data-model section. |

## The problem

A pure-Ollama user is a first-class user; setup treats them as an afterthought.
Ollama is three different products:

1. **Local** — models that run on this machine. The most common flow, and the
   one that depends entirely on RAM/GPU.
2. **Cloud** — big models run on ollama.com under a subscription, proxied
   through the same local daemon and the same `ollama list`. Useful, and the
   plan gates *which* models you may actually call.
3. **Resale via an API key** — Ollama as a keyed reseller. Not modeled here, and
   deliberately so: that is an OpenRouter-shaped problem and OpenRouter is the
   backend we would add for it, later, as its own backend.

Pix models none of that. It has one code path that reads `ollama list`, matches
ids against the catalog, and declares everything it saw **verified**. That is
how `ollama/kimi-k3:cloud` — listed on every signed-in machine, callable only
with extra-usage balance — got bound and routed to the `overlord` intent, where
it 401'd at call time. The catalog has since retired it
(`services/host/routing/defaults/models.json`, `available: false`, with a note
explaining exactly this), but **the mechanism that trusted it is unchanged**, so
the next gated cloud model reproduces the incident.

What we want: on a pure-Ollama box, the crew is routed to the best models the
user can *actually* call — sized to their machine for local, proven by a real
request for cloud.

## What is broken today (file:line evidence)

**1. `Verified` is asserted from a listing, never from a call.**
`configureOllamaInference` (`services/host/cmd/pix/inference.go:172`) runs
`ollama list`, keeps the first whitespace field of each row
(`inference.go:180-190`), and for every catalog model whose id appears in that
set writes `Available: true, Verified: true` (`inference.go:204`). No request is
ever made. Every other backend earns `Verified` through
`verifyDirectInference` (`inference.go:494`), which makes a bounded,
model-specific generate and promotes bindings independently. This violates
safety invariant **13** ("success words are earned by a probe"), and it is the
whole kimi-k3 class.

It gets worse downstream: `inferenceBindingCallable` (`inference.go:606`)
returns `ok && (backend.Auth != "1password" || binding.Verified)`
(`inference.go:611`). The Ollama backend is written with `Auth: "none"`
(`inference.go:196`), so an Ollama binding is callable **regardless of
`Verified`**. Honest verification alone would not fix this; the callability rule
has to change with it.

**2. The registry's `local` flag is ignored.** `routing.Model.Local`
(`routing/routing.go:41`) distinguishes a model that runs here from one that
runs on ollama.com. The bind loop at `inference.go:196-207` filters only on
`m.Provider != "ollama"` and `m.Available`, so local and cloud are one
undifferentiated path: a local-only user gets cloud bindings, and vice versa.

**3. There is no hardware probe anywhere in `services/host`** (`grep -rn
"memsize\|MemTotal\|NumCPU\|meminfo\|VRAM" --include=*.go services/host` is
empty). Local model choice cannot be sized to a machine that is never measured.

**4. The catalog carries exactly one local rung.** `ollama/qwen3.5:9b` is the
only `local: true` entry in `defaults/models.json`, so a 128 GB M4 Max and a
16 GB Air are offered the same 9B. (Resolved: a 24 GB floor — see the RAM table.)

**5. A local-only user who has not pulled that one model hits a hard error and
setup dies.** `inference.go:209`:

```go
return false, fmt.Errorf("Ollama is healthy but none of its installed models match the Pix catalog")
```

That propagates out of `setupChooseInference` (`inference.go:102-104`) into the
`keys` mutation step, which is `fatal: true` (`setup.go:662-665`). The most
common flow — "I installed Ollama, I have not pulled anything Pix knows about" —
has the worst outcome in the whole setup.

**6. Nothing detects an Ollama Cloud account.** Entitlement is inferred from
`ollama list` containing a `:cloud`-tagged row, which is exactly the inference
that failed. **7. There is no reseller flow, and there should not be.**

Two more things that are not bugs but constrain the fix:

- `setupChooseInference` returns early when `len(cfg.Inference.Backends) > 0`
  (`inference.go:52-60`) — that is the **pack** case, and pack exclusivity flows
  through `ExclusiveSource`/`Source` (`inference.go:588-600`,
  `config/config.go:251,266`). Any new rule must not make a pack-declared
  binding uncallable.
- `scripts/check-endpoint-literals.sh` fails the build on a `127.0.0.1:11434` or
  `localhost:11434` literal in Go outside the resolver's allowlist. The new
  probe must resolve its endpoint with `effectiveOllamaEndpoint`
  (`readiness_ollama.go:71`).

## Design

### D1. Two flows, one prompt, stable tokens

Today's prompt (`inference.go:72-77`) is a flat list, and `3` is printed as
"Custom gateway" whether or not the Ollama row appeared:

```
How should Pix run models? (choose one or more)
  1. API key (default)
  2. Ollama
  3. Custom gateway
Choose [1]:
```

**Chosen:** keep the flat multi-select and the existing token numbering, and add
Ollama Cloud as a fourth token. Verbatim new text (the two Ollama rows print
only when the binary + daemon probe at `inference.go:64-70` succeeded):

```
How should Pix run models? (choose one or more, comma-separated)
  1. API key (default)     Anthropic / OpenAI / Google keys, resolved from 1Password
  2. Ollama local          models that run on this machine
  3. Custom gateway        an OpenAI-compatible endpoint you host
  4. Ollama Cloud          large models on your ollama.com subscription
Choose [1]:
```

When the cloud row is shown and `ollama list` already contains `:cloud`-tagged
rows, one hinting line is appended below the menu. Its wording is chosen so it
claims nothing:

```
  (this machine lists 4 cloud model(s); Pix proves which ones your plan can call)
```

Parsing extends the switch at `inference.go:86-97` with a fourth case, keeping
the word aliases as the stable contract:

| token | aliases | meaning |
| --- | --- | --- |
| `1` | `api` | direct provider keys |
| `2` | `ollama`, `ollama-local`, `local` | Ollama local |
| `3` | `gateway` | custom gateway |
| `4` | `ollama-cloud`, `cloud` | Ollama Cloud |

`2,4` selects both, which is the natural "I have a Mac and a subscription"
answer. `2` alone is the most common flow and must be perfect.

*Rejected:* (a) a nested sub-prompt after choosing `2` ("local, cloud, or
both?") — reads better, but costs a prompt round in a flow that budgets prompts
(`prompts.reserve`, `setup.go:828`) and makes the non-interactive path
asymmetric. Runner-up. (b) Renumbering so the Ollama rows are adjacent — breaks
every script that answers `3` for a gateway; ugly ordering is cheaper than a
silent behavior change. (c) A separate `pix ollama setup` verb — splits the one
inference question in two, which is how this mess started.

### D2. Hardware tiering — `readiness_hardware.go`

New file, deliberately named to sit beside `readiness_ollama.go`, whose header
rule it inherits verbatim: **an inference informs remediation and offers, it can
never produce a verdict.** A RAM reading is not a probe of anything Pix ships,
so it may never render `ready` (invariant **13**).

```go
// hostMemory is the probed physical-memory fact, the fraction of it a model
// runtime may plan on, and where the number came from. OK=false means the
// machine could not be sized: callers degrade to the floor rung, never up.
type hostMemory struct {
	TotalGB  float64
	UsableGB float64
	Source   string // "sysctl hw.memsize" | "/proc/meminfo MemTotal" | ""
	OK       bool
}

// probeHostMemory reads total physical memory through the shellEnv seams, so it
// is fakeable in tests and never links cgo. darwin: `sysctl -n hw.memsize`
// (bytes). linux: /proc/meminfo MemTotal (kB). Any other GOOS: OK=false.
func probeHostMemory(env shellEnv) hostMemory

// usableFraction is the share of total RAM a model runtime may plan on, by OS.
func usableFraction(goos string) (float64, bool)

// hardwareCheck renders the doctor row. It is ALWAYS a note and NEVER ready.
func hardwareCheck(mem hostMemory) []check
```

`probeHostMemory` uses `env.probe`/`env.run` on darwin and `env.readFile` on
linux — both already exist on `shellEnv` (`doctor.go:31,35,54`), so no new seam
is required for the reading itself.

**Usable fraction.** macOS unified memory lets the GPU address most of RAM but
not all of it, and the user also has a browser open. The darwin number is
**two-tier**, because the default GPU wired-memory limit
(`sysctl iogpu.wired_limit_mb`, `0` = system default) is itself two-tier:
smaller Apple Silicon machines cap the GPU working set near ⅔ of RAM, larger
ones near ¾. A single 0.75 over-promises on exactly the machines that can least
afford it (this was review finding **S1**):

| GOOS | total RAM | fraction | why |
| --- | --- | --- | --- |
| `darwin` | ≤ 36 GB | **0.67** | the default Metal working-set ceiling on small unified-memory machines; 0.75 exceeds it and the runtime spills to the CPU path or swaps |
| `darwin` | > 36 GB | **0.75** | the larger-machine default, and it still leaves ≥ 12 GB for the OS + editor + browser |
| `linux` | any | **0.60** | no unified-memory guarantee, discrete VRAM is not probed in v1, and CPU inference contends with the desktop. Deliberately conservative — see Open questions |
| anything else | — | — | `OK=false` |

**Weights + KV cache → required usable.** Weights alone are not the working set.
A GQA model's KV cache is on the order of
`2 (K,V) × layers × kv_heads × head_dim × 2 bytes` per token — tens to hundreds
of KB per token — so the context you plan for is a first-class term, not
rounding error. Rev 1 used `ceil(on_disk × 1.15 + 1)`, which gave the 27B about
3.5 GB of total non-weight budget while the catalog advertised **256K context**.
That is the arithmetic bug behind S1. The formula is now:

```
min_ram_gb = ceil(on_disk_gb * 1.15                    // weights + quant/runtime slack
                + ctx_budget_tokens * kv_gb_per_token  // the context we PLAN for
                + 1.0)                                 // process + graph overhead
```

`ctx_budget_tokens` is **declared per rung** and is load-bearing in three
places, which is what keeps the number honest:

1. it is the rung's `context_window` in `defaults/models.json` — a local rung
   **does not advertise 256K**. 256K is the architecture's maximum; the catalog
   entry states the context we sized RAM for. `compileInferenceRuntime`
   (`inference.go:645`) copies `m.ContextWindow` straight into the runtime
   manifest, so this is the number the sandbox actually plans against.
2. it is the `num_ctx` the D4 probe sends, so the probe loads the model at the
   same size the gate priced.
3. it is the `num_ctx` the Ollama bridge sends for a local rung.

A user who wants a 128K local session raises it themselves and owns the swap;
Pix does not offer a context it did not budget RAM for.

**The ladder.** Rungs, on-disk sizes, declared context budget, KV cost per
token, and the derived gate. `kv_gb_per_token` is the published
layers/kv-heads/head-dim arithmetic above at fp16, rounded up to the next 16
KB/token; it is hand-maintained beside the scorecard, same posture:

| rung | on-disk | `ctx_budget_tokens` | KV/token | KV total | `min_ram_gb` (shown) |
| --- | --- | --- | --- | --- | --- |
| `qwen3.5:4b` | 3.4 GB | 8,192 | 32 KB | 0.25 GB | ceil(3.91 + 0.25 + 1) = **6** |
| `qwen3.5:9b` | 6.6 GB | 16,384 | 48 KB | 0.75 GB | ceil(7.59 + 0.75 + 1) = **10** |
| `qwen3.5:27b` | 17 GB | 32,768 | 96 KB | 3.00 GB | ceil(19.55 + 3.00 + 1) = **24** |
| `qwen3.5:35b` | 24 GB | 32,768 | 128 KB | 4.00 GB | ceil(27.60 + 4.00 + 1) = **33** |

(1 GB = 2³⁰; e.g. 32,768 × 96 KB = 3.0 GB, 32,768 × 128 KB = 4.0 GB.)

**A hard 24 GB floor sits above the arithmetic.** Below `localFloorTotalGB`
(24 GB total, whatever the usable budget computes) Pix offers NO local model and
names Ollama Cloud instead. This is a product decision that deliberately
overrides the fits-in-usable-RAM rule, taken by the owner after the review:

> The rule alone said a 16 GB Mac should run the 9B — 16 × 0.67 = 10.7, over its
> 10 GB gate. That is true only of an idle machine. The 9B wires ~9.3 GB,
> leaving ~6.7 GB for macOS, a browser, an editor and the agent itself, and the
> setup probe passes anyway because it runs during the one idle moment the
> machine ever has. The user meets the thrash mid-session, when a probe can no
> longer save them. Below the floor the honest answer is Cloud, not a model
> small enough to fit but too small to code with.

An UNMEASURABLE machine also gets nothing, reversing rev 2's floor-rung offer:
combined with the floor, offering the smallest model to an unsized box hands a
local model to exactly the machines the floor exists to protect, since an
unmeasured machine is likelier small than large.

That makes the 4b rung unreachable by the offer path on every machine (a 24 GB
box already clears the 9b). It stays in the catalog as a routing target for a
user who pulled it themselves — an already-pulled model is bound as a candidate
and judged by the probe, not the gate.

**Total RAM → top offered rung** (`usable = total × fraction`; the top rung is
the largest whose `min_ram_gb ≤ usable`, and only at/above the 24 GB floor):

| total RAM | darwin usable | darwin rung | linux usable | linux rung |
| --- | --- | --- | --- | --- |
| 8 GB | 5.4 (0.67) | **none** — below floor | 4.8 | **none** — below floor |
| 16 GB | 10.7 (0.67) | **none** — below floor | 9.6 | **none** — below floor |
| unmeasurable | — | **none** — say so | — | **none** — say so |
| 24 GB | 16.1 (0.67) | `qwen3.5:9b` | 14.4 | `qwen3.5:9b` |
| 32 GB | 21.4 (0.67) | `qwen3.5:9b` | 19.2 | `qwen3.5:9b` |
| 36 GB | 24.1 (0.67) | `qwen3.5:27b` | 21.6 | `qwen3.5:9b` |
| 48 GB | 36.0 (0.75) | `qwen3.5:35b` | 28.8 | `qwen3.5:27b` |
| 64 GB | 48.0 (0.75) | `qwen3.5:35b` | 38.4 | `qwen3.5:35b` |
| 128 GB | 96.0 (0.75) | `qwen3.5:35b` | 76.8 | `qwen3.5:35b` |

The headline correction: **a 32 GB Mac is offered the 9b, not the 27b.** Rev 1's
table is precisely the machine the reviewer used to demonstrate the co-load
thrash, and the corrected arithmetic removes it as a candidate before any probe
runs — which is also half of the B1 fix, because the local probe set shrinks to
rungs that actually fit.

The 8 GB rows are the honest branch, and they must exist: nothing on the ladder
fits, so Pix offers no local model and says exactly that, pointing at Ollama
Cloud or an API key instead of pulling something that will swap. 8 GB darwin
moved into this branch under the corrected arithmetic, and that is the right
answer — a 3.4 GB model plus an 8 KB-context cache plus macOS itself does not
leave a working machine.

**Degradation when the probe fails.** `mem.OK == false` offers **the floor rung
only** (`qwen3.5:4b`, 6 GB usable — the smallest thing on the ladder, not the
smallest thing that fits an unmeasured box), labeled:

```
  could not size this machine (sysctl hw.memsize failed) — offering the smallest local model only
```

Unknown RAM never means "offer the 122B", and it never means "offer nothing"
either: the floor rung asks for 6 GB usable at an 8K context, which is the
smallest ask on the ladder, and the pull still requires the existing consent
(D5) — so a wrong guess costs a declined prompt and a 3.4 GB download at worst,
not a 24 GB one. Note this is genuinely a *guess*, not a fit: with `OK == false`
we do not know that 6 GB is available. That is why the floor rung is offered
rather than pulled, and why the offer line says "could not size this machine"
instead of naming a budget it does not have.

**Doctor row** — a note, never green, never a `todo` (there is nothing to fix):

```
  · hardware      48 GB (usable ~36 GB, sysctl hw.memsize) — informs local model offers; not a readiness verdict
```

*Rejected:* (a) probing free/available memory rather than total — a snapshot of
an unrelated moment; a machine with a browser open would be offered a smaller
model forever. (b) Probing GPU/VRAM via `system_profiler`/`nvidia-smi` — slow,
format-unstable, and on Apple Silicon it is the same unified pool. Reconsider
for discrete-GPU Linux. (c) Letting the hardware reading pick and pull on its
own — a verdict from an inference, which the file it lives next to forbids.
(d) Keeping the 256K context on local rungs and capping only `num_ctx` at call
time — the catalog would still advertise a context the machine cannot hold, and
`compileInferenceRuntime` would ship it to the sandbox as `ContextWindow`. The
number has to be right where it is read.

### D3. Cloud entitlement — one probe, two labeled guesses

The kimi-k3 incident proves the shape of this problem: the account was signed
in, the model was listed, and the call still 401'd. **Entitlement is per model,
and only a call can prove it.**

| signal | class | what Pix does with it |
| --- | --- | --- |
| `ollama list` contains `:cloud` rows | **guess** | shows the hint line in D1 and builds cloud *candidates* |
| an `ollama` account/auth subcommand's output | **guess**, and rejected outright | not used |
| a bounded generate against the exact cloud tag through the local daemon | **probe** | the only thing that sets `Verified` |

We do not shell out to an Ollama sign-in/whoami verb, for three reasons in order
of weight: it does not prove per-model entitlement (kimi-k3 again); the
subcommand surface varies by version, so absence is indistinguishable from "not
signed in"; and an auth verb can start a browser flow inside a setup transaction
that must stay predictable.

Consequently there is **no stored "has a cloud account" state**. There is a set
of cloud bindings, each of which is either verified or not. Setup reports:

```
  ollama cloud: 3 of 4 model(s) answered; kimi-k3:cloud refused the request (HTTP 401) — not bound
```

If cloud was selected and **zero** cloud bindings verify, and no other backend
produced a callable binding, setup fails with the same shape the direct-key path
already uses (`setup.go:702-711`):

```
Ollama Cloud was selected, but no cloud model answered a request:
  ollama/deepseek-v4-pro:cloud: endpoint rejected the request (HTTP 401)
Sign in with `ollama signin`, then re-run `pix setup`.
```

That is remediation text naming a command the user runs — not Pix running it.

*Rejected:* (a) treating a `:cloud` listing as entitlement — the bug. (b) One
"is cloud alive?" probe generalized to every cloud model — exactly the
generalization that failed; entitlement is per model, per plan tier. (c) Reading
`~/.ollama/id_ed25519.pub` for a signed-in identity — a key on disk proves a key
on disk.

### D4. `verifyOllamaInference` — mirror the direct path

```go
// verifyOllamaInference earns Verified for ollama bindings with an actual
// model-specific request through the RESOLVED endpoint. Every binding is
// checked independently. CLOUD probes run concurrently (they are network round
// trips and hold no local resource). LOCAL probes are SERIALIZED and unload
// after themselves: two concurrent generates make Ollama co-load two sets of
// weights, which either exhausts the memory budget this file just computed or
// serializes the loads anyway behind a timer that started at dispatch. Mirrors
// verifyDirectInference in structure, not in concurrency.
func verifyOllamaInference(cfg *config.Config, env shellEnv) (attempted, verified int, failures []string)

// liveOllamaInferenceProbe posts ONE minimal generate to endpoint/api/generate.
// endpoint is always supplied by effectiveOllamaEndpoint; this function never
// spells an address of its own (scripts/check-endpoint-literals.sh).
func liveOllamaInferenceProbe(endpoint, model string, timeout time.Duration) error
```

New `shellEnv` seam, documented with the same rule as `identityProbe`
(`doctor.go:65-71`) — a nil prober renders bindings **unverified**, never a
silent real network call:

```go
// ollamaInferenceProbe makes ONE bounded, model-specific request against the
// resolved Ollama endpoint. Nil in tests that do not fake it, which leaves
// every ollama binding unverified — never a silent real call, never a false
// verified.
ollamaInferenceProbe func(endpoint, model string, timeout time.Duration) error
```

Request: `POST {endpoint}/api/generate` with
`{"model":"<tag>","prompt":"Reply OK","stream":false,"keep_alive":0,"options":{"num_predict":8,"num_ctx":<rung ctx_budget_tokens>}}`,
no auth header (the local daemon owns any cloud credential; Pix stores none).
Bodies are drained into `io.Discard` behind a `LimitReader` and never echoed,
same as `liveDirectInferenceProbe` (`inference.go:565-568`).

`keep_alive: 0` is load-bearing, not tidiness: it tells the daemon to unload the
model as soon as the response is written, so probe *n+1* starts against a free
memory budget instead of stacking on probe *n*'s resident weights. `num_ctx` is
the rung's declared budget (D2), so the probe allocates the same KV cache the
gate priced — a rung that cannot hold its own declared context fails here, which
is exactly when we want to find out.

**Probe scheduling (review blocker B1).** Rev 1 said "probes run concurrently, so
the wall-clock bound is one probe timeout rather than N", which is true of cloud
APIs and false — dangerously — of local weights. Two concurrent local generates
on a machine sized for one either thrash, or get serialized inside Ollama while
both 90 s timers run from *dispatch*, so the second reports a timeout it never
actually got a turn to spend. Under the new `bindingNeedsHostProof` rule a
timeout is not cosmetic: the model is removed from the callable surface
(`inference.go:364,645,728`). Rev 1 therefore traded "binds a model that cannot
answer" for "un-binds a model that can", which is the worse trade.

**Chosen:** serialize the local set, in this exact shape.

| property | value | why |
| --- | --- | --- |
| cloud probes | concurrent, bound by `ollamaCloudProbeTimeout` | pure network; no local resource is held |
| local probes | **strictly serial**, one in flight | one model resident at a time is the only schedule the D2 budget was computed for |
| local order | **descending rung** (largest fitting first) | the top rung is what the roster and the bridge will actually use; if a budget is going to run out, it must not run out on the model that matters |
| local unload | `keep_alive: 0` per probe | the next probe starts from a free budget, so the D2 sizing holds probe-to-probe |
| per-probe timeout | 90 s | a cold 24 GB read + graph build, with the queue empty because we serialized |
| local wall budget | `ollamaLocalProbeBudget = 300 s` total | four pulled rungs × 90 s is a pathological box, not a setup a user should sit through |
| beyond budget | `not probed`, a THIRD state | never rendered as a failure — see below |
| budget enforcement | a probe is **never started** unless its full 90 s fits the remaining budget; an in-flight probe is never cut short | the budget can therefore never *manufacture* a timeout, which is the failure B1 is about |

```go
const (
	ollamaCloudProbeTimeout  = 20 * time.Second  // network round trip only
	ollamaLocalProbeTimeout  = 90 * time.Second  // ONE cold load, with nothing queued ahead of it
	ollamaLocalProbeBudget   = 300 * time.Second // total for the serialized local set
)
```

**Probe budget, stated plainly:** wall clock ≈ `max(cloud probe) + Σ(local
probes)`, capped at `20 s + 300 s`. The common box (one fitting rung, already
pulled) is **one** ~10-40 s probe. The worst realistic box (a 64 GB Mac with 4b,
9b, 27b and 35b all pulled) spends up to 300 s, and setup prints a live line per
model so the pause is legible rather than a hang:

```
  verifying 4 local ollama model(s), one at a time (each is loaded and unloaded) ...
    qwen3.5:35b   ok (86s)
    qwen3.5:27b   ok (74s)
    qwen3.5:9b    ok (81s)
    qwen3.5:4b    not probed — 59s left of the 300s local budget, less than one probe's 90s
```

(A fast box with all four pulled finishes in well under a minute and prints four
`ok` lines; the arithmetic above is the pathological case, shown because it is
the one that has to degrade honestly.)

**`not probed` is not `failed`.** A candidate the budget never reached keeps
`Verified: false` (so it is not callable — the rule does not bend) but carries
its own reason string, is excluded from the `attempted` count that drives the
zero-verified check (D5/S2), and doctor renders it as `not probed: pix setup`
rather than as a rejection. Conflating the two would re-create the exact
reviewer complaint one level down: a healthy model reported as broken.

*Rejected for B1:* (a) probing **only** the chosen rung — cheapest, and it was
the runner-up. It leaves every other pulled-and-fitting rung permanently
uncallable, which silently shrinks the roster of a user who deliberately pulled
two models. Serial + `keep_alive: 0` + descending order gets the same worst-case
protection without that cost. (b) Starting each local timer at *request-accept*
rather than dispatch — the reviewer's other alternative, and it is the right
instinct, but HTTP gives us no accept signal short of streaming and watching for
the first token, and `stream: false` is what keeps the probe body trivial.
Serialization makes the question moot: with one request in flight there is
nothing to wait behind, so dispatch *is* accept.

**What a failure does to the binding:** `Verified` stays `false` and the binding
stays in config as a candidate (`Available: true`), exactly like a failed direct
probe. What changes is that an unverified Ollama binding is no longer callable.
`inferenceBindingCallable` (`inference.go:606`) gets its rule from a named
predicate instead of an inline `Auth` test:

```go
// bindingNeedsHostProof reports whether Pix can — and therefore must — prove
// this binding from the host before calling it callable. Pack-declared bindings
// are exempt: a pack's authority is the sandbox smoke test (see
// enableDeclaredInferenceBindings), and sbx-session auth cannot be faithfully
// replayed by a host HTTP probe.
func bindingNeedsHostProof(cfg *config.Config, b config.InferenceModelBinding) bool {
	if b.Source != "" {
		return false
	}
	backend, ok := cfg.Inference.Backends[b.Backend]
	return ok && (backend.Auth == "1password" || backend.Driver == "ollama")
}

// ... and inferenceBindingCallable's tail becomes:
//   return !bindingNeedsHostProof(cfg, binding) || binding.Verified
```

That is the change that makes the kimi-k3 class structurally impossible: a gated
model may stay in the catalog, get listed, and get bound as a candidate — and
still never reach `routing.json`, because `compileInferenceRuntime`
(`inference.go:645`) and `callableRuntimeModels` both gate on
`inferenceBindingCallable`.

**What setup prints** — per model, and **only on partial failure**, matching
today's direct-key line (`setup.go:715-718`). A routine "N model(s) verified"
line from a mutation step would be a success claim rendered by the mutation
instead of by the post-mutation probe, which is what AC-P0-302 forbids (review
nit **N2**). The progress lines above (`verifying …`, per-model `ok`) are
diagnostics about work in progress, not verdicts about an axis, and they are the
only thing printed when everything succeeds:

```
  inference: 4 model(s) verified; 1 candidate(s) unavailable or unauthorized (ollama/kimi-k3:cloud: endpoint rejected the request (HTTP 401))
```

**Doctor (review finding S3).** `inferenceBindingCallable` has five callers, not
three: `inference.go:364` (`callableRuntimeModels`), `:645`
(`compileInferenceRuntime`), `:728` (the credential set) — and **also**
`doctor_providers.go:126` (`configuredBindingForModel`, which backs the
`run_intent` row) and `:155` (`configuredInferenceSummary`, which backs
`inferenceCoreCheck`). Rev 1 missed the last two, and they mis-remediate the new
state: on a pure-Ollama box in the declined-pull state,
`configuredInferenceSummary` returns 0, so `inferenceCoreCheck`
(`doctor_providers.go:137-147`) falls through to `modelKeyCoreCheck`, whose fix
is `modelKeyFixCmd` = `pix secret set ANTHROPIC_API_KEY …`
(`doctor_providers.go:119`) — a cloud-provider-key remediation for a
not-pulled-a-model problem. The `run_intent` row stops matching too, for the
same reason.

So doctor gains one branch and one helper:

```go
// pullModelsFixCmd is the ONE copy-pasteable command for the state this design
// creates: ollama candidates are bound, none has passed a probe, and the reason
// is almost always "the weights are not on disk". It is NOT a provider-key fix.
const pullModelsFixCmd = "pix setup --pull-models"

// unverifiedOllamaCandidates returns bound-but-unproven ollama bindings — the
// declined-pull state. Non-empty means the fix is a pull, never a key.
func unverifiedOllamaCandidates(cfg *config.Config) []string
```

- `inferenceCoreCheck`: when `count == 0` **and**
  `len(unverifiedOllamaCandidates(cfg)) > 0`, return
  `{requirement: core, verdict: verdictTodo, detail: "N local model candidate(s)
  bound but unproven (not pulled, or the probe failed)", todo: pullModelsFixCmd}`
  instead of falling through. Falling through to `modelKeyCoreCheck` stays the
  behavior only when there are no ollama candidates at all — i.e. the host really
  does need a provider key.
- the `run_intent` row (`doctor_providers.go:126`): when the intent's model does
  not match a *callable* binding but does match an **unverified ollama** one, the
  detail names the pull, not the provider key.

This also keeps the promise D4 makes about keeping failed bindings as
candidates: they exist so that doctor can explain the absence, and that only
pays off if doctor actually reads them.

**Roster (review nit N1).** Moving `configureModelRoster`'s candidate filter from
`b.Available && inferenceBindingTopologyAllowed(...)` to
`inferenceBindingCallable(cfg, b)` changes the non-interactive `--models X` path:
if `X` is bound but failed (or never reached) its probe, `canonicalize` now
returns `model %q is not available through the selected runtime`
(`inference.go:262-264`) where today it succeeds. That is intended — a scripted
setup that names a model which cannot answer should fail loudly at setup rather
than produce a roster whose entries 401 at call time — but it is a **behavior
change to a non-interactive contract**, so it is called out here, gets a test,
and the error string is worth widening to name the reason (`… is bound but has
not passed a probe: pix setup --pull-models`).

**Ordering.** Verification cannot live inside the `keys` step, because the model
it must call may not be pulled until the `models` step (`setup.go:816`). So:

1. `keys` step — bind candidates (`Verified: false`), record the wanted local
   rung, `cfg.Save()`. **No hard error when nothing is bound yet.**
2. `models` step (unchanged) — pulls under its existing consent (D5).
3. **New `inference` step, after `models`** — `verifyOllamaInference`, then
   `configureModelRoster`, then `cfg.Save()`, then the zero-verified branch
   (D5/S2, which is where the `fatal` flag and the decline branch are pinned
   down).

`setupMutationOrder` (`setup.go:634-638`) becomes
`{"keys", "config", "pack", "mcp", "knowledge", "identity", "gworkspace",
"models", "inference"}`, and its comment is now wrong in two ways that must be
fixed in the same change (review nit **N2**): `models` is no longer last, and
`gworkspace`/`models` are no longer "the only two steps that can ask the user a
question" — the `inference` step inherits the roster prompt from `keys`. New
comment: *"gworkspace, models and inference sit at the end because they are the
only steps that talk to the user; models is second-to-last because it is the
only step that can cost gigabytes, and inference is last because it can only
judge what models left behind."* The AC-P0-303 order test gains the ninth
element, and the roster prompt must be reserved from `prompts.reserve`
(`setup.go:828`) in the `inference` step rather than the `keys` step, or the
prompt budget silently double-counts.

Moving `configureModelRoster` after verification also fixes a smaller latent
bug: today the roster prompt (`inference.go:218`, candidate filter at
`inference.go:228-236`) offers models that have not been proven, so a user can
pick a model that then fails. The roster's candidate filter changes from
`b.Available && inferenceBindingTopologyAllowed(...)` to
`inferenceBindingCallable(cfg, b)`. The direct-key path moves to the same
order in the same change (bind → verify → roster), which costs at most one
8 s probe timeout before the prompt; setup prints `verifying N model(s) ...`
so the pause is legible.

*Rejected:* (a) `ollama show <tag>` — answers "is this model known", the listing
question again. (b) `/v1/models` — same class; an inventory endpoint is not an
entitlement check. (c) Deleting a failed binding — keeping it as an unverified
candidate is what lets `doctor` and the summary explain *why* an expected model
is missing from the roster.

### D5. A local-only user is never hard-failed

Delete the error at `inference.go:209`. The replacement path, when Ollama local
is selected and no catalog local model is present:

1. Probe the machine (D2) and pick the top rung that fits.
2. Set `cfg.OllamaBridgeModel` to that rung's tag.
3. Bind it as a candidate (`Available: true, Verified: false`).
4. Return successfully. Setup continues.

Step 2 is the whole trick: **there is no new consent mechanism.**

`cfg.OllamaBridgeModel` is already one of the three roles `setupLocalModels`
classifies and pulls (`setup_models.go:108`, the `bridge` role), so the rung
flows through the machinery whose header comment already spells out the consent
contract: `--pull-models` is explicit consent in any mode and the only consent a
non-interactive setup honors; a bare `--yes` never approves downloads;
interactive gets **one** aggregate, default-No prompt naming every
confirmed-missing tag with a disk-size warning; an unverifiable probe is never
"missing" and is never pulled.

The prompt the user sees is the existing one, now carrying the rung — but its
**header line must stop saying "optional"** (review nit **N3**). Today's literal
is `"Missing local Ollama models (optional — fact capture, semantic recall, the
sandbox bridge)"` (`setup_models.go:153`), a static string written when all three
roles really were progressive enhancement. On a pure-local box the `bridge` tag
is now the **only model Pix can call at all**, and telling the user it is
optional is the same class of untrue statement this whole design exists to
delete. The header becomes role-derived:

```
Missing local Ollama models:
  qwen3.5:9b        (bridge, inference)  — REQUIRED: the only model Pix can call on this machine
  nomic-embed-text  (embed)              — optional: semantic recall
Pull 2 models now? Each download can be several GB of network and disk. [y/N]
```

Mechanically: `modelReadiness` (`setup_models.go:108`) already carries a purpose
string and a `requirement`; the bridge role's requirement is promoted from
`requirementOptional` to `requirementCore` when the rung is also the inference
binding, and the header/per-line text is rendered from the roles present rather
than hard-coded. The consent mechanics — default No, one aggregate prompt,
`--pull-models` as the only non-interactive consent, `--yes` never approving a
download — are untouched.

**The rung is written to `cfg.OllamaBridgeModel` before consent, and that is
accepted, not fixed.** N3 is correct that a declined pull leaves config naming a
tag that is not on disk. The alternative — write the tag only after the user says
yes — is not implementable in this order: `setupLocalModels` *reads*
`cfg.OllamaBridgeModel` to build its readiness axes (`setup_models.go:108`), so
the tag must exist in config before the step that asks about it. Writing it is
also not a claim: `Verified` stays false, the binding is not callable, and the
three surfaces that could mislead someone all report the truth — the `✗
inference` summary row below, doctor's `axisModelBridge` row (which probes for
the tag and reports it missing), and the new S3 core-check branch whose fix is
`pix setup --pull-models`. A config key naming a not-yet-pulled tag is a
*declared intent*, which is exactly what `ollama_bridge_model` has always been
(its shipped default names a tag no fresh machine has pulled either).

**S2 — the zero-verified branch, and why the decline exits 0.** Rev 1 said
"exits 0" and separately described the new step as carrying "the zero-verified
check", copied from the direct path (`setup.go:706-712`) where it is a hard
error in a `fatal: true` step. Those two statements contradict: a naive port
fails setup on the decline path. Worse, `runSetupMutations` (`setup.go:646-656`)
returns a **non-fatal** step's error too, so `fatal: false` alone does not buy
exit 0. The decline path must not produce an error at all. Pinned down:

- the `inference` step is **`fatal: false`** — it is last, and a probe failure
  must not prevent the receipt/report from rendering the axes it did touch;
- it reads the already-threaded `*setupModelsOutcome` (`setupMutationSteps` takes
  `models *setupModelsOutcome`), which the `models` step has populated by then;
- and it branches:

| condition | returns | exit |
| --- | --- | --- |
| `verified > 0` | `nil` | 0 |
| `verified == 0`, `attempted == 0`, `models.consent ∈ {"prompt-no", "none"}` | **`nil`** — print the honest note, claim nothing | **0** |
| `verified == 0`, `attempted > 0` (models were present and every probe was refused/timed out) | `error` naming each failure | non-zero |
| `verified == 0`, `models.consent ∈ {"--pull-models", "prompt-yes"}` and the pull failed | `nil` — the `models` step already failed non-zero and owns the retry command; a second error would double-report one cause | non-zero (from `models`) |
| cloud selected, `verified == 0` cloud bindings | `error` (D3's text) | non-zero |

`attempted` counts probes that were actually *dispatched*, so a `not probed`
candidate (B1's budget state) never converts a decline into a failure. The
`✗ inference` row and `Core not ready` come from the post-mutation probe, not
from the step's return value — AC-P0-302 intact.

So, on decline, setup **exits 0** with an honest summary (`printSetupSummary`
already has the branch):

```
  ✗ inference    no callable model
Core not ready: configure and verify at least one model.
  next: pix setup --pull-models        (or by hand: ollama pull qwen3.5:9b)
```

Declining a multi-gigabyte download is a decision, not a failure, so it is not a
non-zero exit — that stays reserved for a pull that was consented to and then
failed (`setup_models.go` already does this).

**S5 — what `pix run` then does, and which gate actually fires.** Rev 1 claimed
this state is caught by invariant **6** "with the fix command attached". Both
halves were wrong.

The gate that actually fires is `synthesizeInferenceKit` (`inference.go:673`,
reached from `run.go:221`): with at least one binding in config but zero callable
ones, `compileInferenceRuntime` yields an empty manifest and it returns
`"inference is configured but no model binding passed its probe"`, which `run.go`
prints as `pix: inference: …` and exits 1 — **with no remediation attached**. So
the fix command is not a claim about existing behavior; it is a required part of
this change:

```go
return "", fmt.Errorf("inference is configured but no model binding passed its probe; pull a local model with `pix setup --pull-models`, or re-run `pix setup` to re-verify")
```

And the invariant citation is corrected. Invariant **6** is about the **model-key**
probe — `run` refuses only on a *positive* `sbx secret ls` answer, and a
transient failure of that command proceeds. That mechanism does not fire here.
What transfers is its **spirit**, and this gate satisfies it for a different
reason: the refusal is computed from Pix's own config plus a completed probe
record, not from a subprocess that can fail transiently, so there is no "unknown"
state to mistake for a "no". A binding that could not be probed is `Verified:
false` *and* recorded as unprobed, which is a positive local fact, not a failed
question. Invariant 6's failure mode — refusing to launch because a probe
errored — is structurally absent.

One rung, chosen by RAM, is what setup offers. Not a menu of four.

*Rejected:* (a) a second consent prompt owned by the inference step — two
consent mechanisms for downloads is how you end up pulling 24 GB from a `--yes`.
(b) Auto-pulling the floor rung because it is "only 3.4 GB" — silent downloads
are a category we do not have. (c) Keeping the hard error with better text — the
user did nothing wrong.

### D6. Router degradation on a pure-Ollama box

**What happens today.** `Resolve` (`routing/resolve.go:47`) applies every hard
constraint at once (step 2), and on an empty feasible set falls through to step
4: rank *all* scored+available candidates by the objective, `ConstraintsMet =
false`, `Reason` naming the degradation. So a pure-Ollama box does not starve —
`RegistryForBindings` (`routing/routing.go:63`) already marked every unbound
cloud model unavailable, and `overlord` (`providers: ["openai"]`) degrades to the
best available Ollama model.

The problem is that step 4 is a **cliff** — it drops every constraint at once. On
an Ollama-only box `breadth` (`objective: cost`, `min_accuracy: 0.60`,
`max_latency_ms: 30000`) sees every local candidate at `cost_usd: 0.0`, so the
cost tiebreak falls through to *accuracy descending* (`rankBy`,
`resolve.go:172-182`) and a `fanout` of eight parallel children lands on the
**largest local model on the machine**. The worst possible answer for a cheap
breadth pass.

**Chosen:** a staged relaxation ladder, dropping one constraint class at a time,
in this documented order:

| stage | dropped | rationale |
| --- | --- | --- |
| 0 | nothing | today's step 2 |
| 1 | provider allowlist | vendor diversity is a *preference* encoded as a constraint; it is the first thing that stops making sense when there is one vendor |
| 2 | accuracy floor | a floor calibrated against frontier cloud models is unmeetable locally; better a weaker model than the wrong-sized one |
| 3 | cost ceiling | on a subscription/local box, per-task USD is a proxy that has stopped meaning anything |
| 4 | latency ceiling (i.e. nothing left) | last on purpose: latency is the axis that still protects the user's wall-clock time on a laptop, and it is what keeps `breadth` off the 35B |

```go
// Decision gains:
//   Relaxed []string `json:"relaxed,omitempty"` // constraint classes dropped, in the order dropped
// CompiledRoute gains the same field, so routing.json carries it to the sandbox.
// DO NOT bump CompiledRoutingVersion for this. See the data-model section.

// relaxationLadder is the documented order in which hard constraints are
// surrendered when nothing is feasible. Provider first (a preference), latency
// last (it protects the user's time on a local box).
func relaxationLadder() []relaxationStage
```

`Resolve` loops the ladder, stopping at the first stage with a non-empty
feasible set. `ConstraintsMet` is `false` for any stage > 0 (unchanged
semantics), `Relaxed` names what was dropped, and `Reason` reads:

```
overlord: nothing matched provider in [openai]; relaxed provider -> ollama/deepseek-v4-pro:cloud (accuracy 0.91, $0.2800, 24000ms) from 3 feasible of 6 scored
```

The existing step 4 stays as the terminal case (no scored+available candidate
survives even an empty constraint set → the diagnostic fallback), so nothing
about `MaterializeBindings` changes.

This only routes sanely if the scorecard is honest about local latency. A local
rung's `latency_ms_p50` must reflect the machine class it runs on (the shipped
9B is already at 35000 ms, above `breadth`'s 30000 ms ceiling), which is exactly
what keeps stage-1 `breadth` on a small rung or a cloud model. Scoring the new
rungs is therefore part of the change, not a follow-up (D7).

**Visibility: yes, mandatory.** A relaxed route is a route the user did not ask
for, so it must be legible in both surfaces:

- `pix agent ls` — `explainDecision` (`cmd/pix/agent.go:229`) currently prints
  `"%s: nothing matched (%s) -> fallback"` for any `!ConstraintsMet`. New text:
  `overlord: relaxed provider (no openai model is callable here) -> deepseek-v4-pro:cloud`.
- `pix route show` and `routing.json` — already carry `ConstraintsMet` and
  `Reason` per route (`routing/compile.go:16-25,53-59`), so both inherit the
  new reason for free; `Relaxed` is added beside them for machine readers.

*Rejected:* (a) leaving the cliff and special-casing `breadth` — one intent's
symptom, not the disease. (b) Relaxing accuracy before provider — hands a
cross-vendor intent a *worse same-vendor* model while a better one sits right
there. (c) Per-intent relaxation policy in `policy.json` — more knobs than the
problem has.

### D7. Catalog changes

**Local rungs (new, `available: true`).** Three new entries beside the existing
`ollama/qwen3.5:9b`, all Apache-2.0 Qwen 3.5. **`context_window` is the declared
RAM-budgeted context from D2, not the architecture's 256K** — the existing 9b
entry's `context_window` is edited down in the same change, which is a real
(intended) narrowing of what the sandbox is told it may fill:

| id | label | on-disk | `context_window` | `min_ram_gb` | `local` |
| --- | --- | --- | --- | --- | --- |
| `ollama/qwen3.5:4b` | Qwen 3.5 4B (local) | 3.4 GB | 8,192 | 6 | true |
| `ollama/qwen3.5:9b` | Qwen 3.5 9B (local) | 6.6 GB | 16,384 | 10 | true |
| `ollama/qwen3.5:27b` | Qwen 3.5 27B (local) | 17 GB | 32,768 | 24 | true |
| `ollama/qwen3.5:35b` | Qwen 3.5 35B (local) | 24 GB | 32,768 | 33 | true |

Not carried in v1: `0.8b` and `2b` (below the floor of a useful coding agent —
an 8 GB machine gets `4b` with a plain warning, or cloud), `122b` (81 GB reaches
only 128 GB machines, and the download is a bigger ask than the delta earns),
and the `-mlx` variants (arch-specific tags that would double the scorecard for
a speed delta, not a capability delta — see Open questions).

**Cloud models:** unchanged. The current set is already the post-incident set —
`glm-5.2:cloud`, `deepseek-v4-flash:cloud`, `deepseek-v4-pro:cloud`,
`kimi-k2.7-code:cloud`, `qwen3.5:397b-cloud` available; `kimi-k3:cloud` retired
with `available: false`. D4 is what makes leaving a gated model in the catalog
safe rather than dangerous.

**New registry fields** on `routing.Model`, both optional so existing JSON
parses unchanged:

```go
	MinRAMGB    float64 `json:"min_ram_gb,omitempty"`  // minimum USABLE RAM (weights*1.15 + ctx_budget*kv_per_token + 1GB), compared against the probed usable budget — not against total RAM
	DownloadGB  float64 `json:"download_gb,omitempty"` // on-disk size, for the honest size warning in the pull prompt
	KVGBPerTok  float64 `json:"kv_gb_per_token,omitempty"` // fp16 KV cache per token; the term MinRAMGB was computed with, carried so the gate is auditable rather than a magic constant
```

`ContextWindow` already exists and is now doing double duty as the rung's
`ctx_budget_tokens`: it is what `min_ram_gb` was priced for, what the probe sends
as `num_ctx`, and what the bridge sends. One number, three readers, no way for
them to drift.

Plus a small helper so both binaries share the gate:

```go
// LocalRungs returns the registry's available local models, largest first.
func LocalRungs(reg *Registry) []Model

// FitsMemory reports whether m fits a usable-memory budget. A model with no
// declared MinRAMGB never fits: an undeclared requirement is not a small one.
func (m Model) FitsMemory(usableGB float64) bool
```

**Existing catalog tests must keep passing.**
`TestShippedOllamaIDsAreValidOllamaReferences`
(`routing/catalog_test.go:39`) — every new id is `name:tag` with a single colon
after the `ollama/` prefix, so `qwen3.5:27b` passes and a hypothetical
`qwen3.5:27b:mlx` would (correctly) fail.
`TestEveryAvailableModelIsFullyScored` (`catalog_test.go:78`) — each new rung
needs a `scorecard.json` row for **all four** policy task types (`code`,
`reasoning`, `search`, `qa`). That is 12 new rows, hand-entered, following the
principle below. `TestScoredModelsExistInRegistry` (`catalog_test.go:112`) —
nothing removed, so it stays green.

**Scoring principle for the rungs.** Local rungs are scored to **lose on a mixed
box and win only when they are all that is bound**: accuracy scales with size and
stays under the cloud tier (the shipped 9B is `code: 0.68`, `reasoning: 0.72`),
`cost_usd: 0.0`, and `latency_ms_p50` scales with size and with being local (the
9B is already 35000 ms; 27B/35B are materially slower). Availability comes from
bindings, never from the RAM gate — `min_ram_gb` is a **setup-time offer
filter**, so `pix route show` on an unbound box still describes the catalog
truthfully.

## Data model changes

**`routing/routing.go`** — `Model.MinRAMGB` (`min_ram_gb,omitempty`),
`Model.DownloadGB` (`download_gb,omitempty`), `Model.KVGBPerTok`
(`kv_gb_per_token,omitempty`), `Decision.Relaxed` and `CompiledRoute.Relaxed`
(`relaxed,omitempty`).

> **DO NOT BUMP `CompiledRoutingVersion` (review nit N4).** It is `1`
> (`routing/compile.go:12`) and it must stay `1` in this change.
> `extensions/subagents.ts:154` requires an **exact** match
> (`if (parsed.version !== ROUTING_SCHEMA) … return null`) and silently drops
> every agent to "inherit parent model" when it does not match. Every sandbox
> image already built carries the old `ROUTING_SCHEMA`, so bumping the constant
> — the instinctive, tidy thing to do when adding a field — **bricks subagent
> routing in every existing image** until each one is rebuilt and reloaded
> (`make load`, ~1 GB). `Relaxed` is additive and optional; the sandbox reader
> ignores unknown fields. There is nothing to version. If a future change ever
> does need the bump, it ships *with* an image rebuild and a note in AGENTS.md,
> not on its own.

**`defaults/models.json`** — three new local entries plus
`min_ram_gb`/`download_gb`/`kv_gb_per_token` on all four, and `context_window`
edited down on the existing 9b entry (D2/D7). **`defaults/scorecard.json`** — 12
new rows.

**`services/host/config`** — no new keys. `OllamaBridgeModel`
(`config.go:37`, default `qwen3.5:9b`) is reused as the local rung, and it is
already settable through `pix config set ollama_bridge_model <tag>`
(`cmd/pix/config.go:318-325`), which keeps invariant **1** intact: the launcher
writes it through the config API, and a user overrides it with `config set` —
never by hand-editing `config.toml`.

**`config.InferenceModelBinding`** — **two optional fields added** (review
blocker **B2**). Rev 1 said "unchanged" while the migration section promised a
doctor row that distinguishes a listing-set `verified = true` from a
probe-earned one. Those cannot both be true: without provenance the two rows are
bit-identical, so the TODO either fires on every verified ollama binding forever
or never fires at all. Provenance goes on the binding:

```go
type InferenceModelBinding struct {
	// … existing fields …

	// VerifiedBy records HOW Verified was earned. "probe" is the only value this
	// codebase writes; empty on a binding written before provenance existed, which
	// is exactly the legacy listing-derived claim doctor must flag. Decision-
	// bearing: `Verified && VerifiedBy != "probe"` IS the migration predicate.
	VerifiedBy string `toml:"verified_by,omitempty"`

	// VerifiedAt is RFC3339 evidence for the doctor/summary text ("verified
	// 2026-07-14"). NEVER read for a decision in v1 — no staleness expiry, no
	// re-probe trigger. It exists so the row can cite a date instead of asserting
	// one, and adding it now avoids a second config migration later.
	VerifiedAt string `toml:"verified_at,omitempty"`
}
```

Back-compatible by construction: both are `omitempty` on the sparse TOML encode
(invariant **1** — written through `cfg.Save()`, never hand-edited), an old
config parses with both empty, and an old `pix` reading a new config ignores
them. `verifyOllamaInference` and `verifyDirectInference` both set
`VerifiedBy = "probe"` and `VerifiedAt = now` on promotion, and both **clear**
them alongside `Verified` on demotion, so the pair can never outlive the claim
it describes.

`Verified` finally means what its comment already claims ("successful
backend-specific probe, not declaration"), and `VerifiedBy` is how a reader
checks that at rest.

*Rejected for B2:* keying the doctor TODO off the absence of a post-upgrade
setup receipt (`<state-dir>/setup/models.json`, `setup_models.go:282-303`) — the
reviewer's other alternative and a genuinely tempting one, since the machinery
exists. It fails on precision: the receipt records the **pull** decision, not the
verification, it is skipped entirely when Ollama is not installed
(`receiptSetupModels`, `setup_models.go:310`), it lives in the state dir which
`pix state reset` legitimately clears, and it is per-run rather than per-binding
— so it cannot say *which* of five bindings is stale. Provenance belongs on the
thing whose provenance is in question.

## New and changed functions

New — `readiness_hardware.go`: `probeHostMemory`, `usableFraction`,
`hardwareCheck`, `hostMemory` (D2). New — `routing`: `LocalRungs`,
`Model.FitsMemory` (D7). New — `doctor_providers.go`: `pullModelsFixCmd`,
`unverifiedOllamaCandidates`, `legacyVerifiedOllamaBindings` (S3, B2). New —
`inference.go`: `verifyOllamaInference`, `liveOllamaInferenceProbe`,
`bindingNeedsHostProof` (D4), plus:

```go
// ollamaSelection is what the user chose in D1's prompt.
type ollamaSelection struct{ Local, Cloud bool }

// ollamaPlan is what configureOllamaInference decided, for the caller to render
// and for the models step to act on. It contains no success claims.
type ollamaPlan struct {
	Endpoint   string   // resolved via effectiveOllamaEndpoint
	LocalBound []string // catalog ids bound as candidates from the listing
	CloudBound []string
	WantPull   string   // the RAM-appropriate rung handed to setupLocalModels
	SkippedRAM []string // catalog local ids this machine cannot run
	Memory     hostMemory
}

func configureOllamaInference(cfg *config.Config, env shellEnv, sel ollamaSelection, out io.Writer) (ollamaPlan, error)
func ollamaListedModels(env shellEnv) (map[string]bool, error)
func chooseLocalRung(reg *routing.Registry, mem hostMemory) (routing.Model, bool)
```

Changed:

| function | change |
| --- | --- |
| `setupChooseInference` (`inference.go:48`) | new 4-token prompt (D1); passes an `ollamaSelection`; unchanged early return for the pack case at `:52-60` |
| `configureOllamaInference` (`inference.go:172`) | splits local/cloud on `Model.Local`; gates local on `FitsMemory`; binds `Verified: false`; **never returns the "none of its installed models match" error**; sets `cfg.OllamaBridgeModel` to the chosen rung |
| `configureModelRoster` (`inference.go:218`) | candidate filter becomes `inferenceBindingCallable`; runs after verification |
| `inferenceBindingCallable` (`inference.go:606`) | rule moves to `bindingNeedsHostProof`; an unverified non-pack Ollama binding is no longer callable |
| `setupMutationSteps` (`setup.go:659`) | new `inference` step after `models` (`setup.go:816`), `fatal: false`: verify → roster → save → the S2 zero-verified **branch** (not the direct path's hard error) |
| `setupMutationOrder` (`setup.go:634-638`) | ninth element `"inference"`; the "only two steps that can ask a question" comment is rewritten (N2); AC-P0-303 test updated |
| `setupLocalModels` (`setup_models.go:103`) | the pull prompt header stops saying "optional" and is rendered from the roles present; the `bridge` role is `requirementCore` when it is also the inference rung (N3) |
| `verifyDirectInference` (`inference.go:494`) | also writes `VerifiedBy`/`VerifiedAt` on promotion and clears them on demotion, so provenance is uniform across backends |
| `synthesizeInferenceKit` (`inference.go:673`) | the "no model binding passed its probe" error gains the `pix setup --pull-models` remediation (S5) |
| `inferenceCoreCheck` (`doctor_providers.go:137`) | new branch: unverified ollama candidates → `pix setup --pull-models`, instead of falling through to `modelKeyCoreCheck`'s `ANTHROPIC_API_KEY` fix (S3) |
| `configuredBindingForModel` (`doctor_providers.go:120`) | the `run_intent` row distinguishes "no binding" from "an unverified ollama binding" and remediates each correctly (S3) |
| `printSetupSummary` (`setup.go:~880`) | adds the per-backend cloud/local verification line |
| `routing.Resolve` (`resolve.go:47`) | staged relaxation ladder replaces the single cliff; populates `Relaxed` |
| `explainDecision` (`agent.go:229`) | renders `relaxed <class>` instead of a bare `-> fallback` |

## Test plan

Go tests under `services/host/...`. Each row names the mutation it catches.

| test | mutation it catches |
| --- | --- |
| `TestConfigureOllamaInferenceBindsUnverifiedCandidates` — listing-derived bindings have `Verified == false` | re-introducing `Verified: true` at `inference.go:204` |
| `TestVerifyOllamaInferencePromotesOnlyAnsweringModels` — prober succeeds twice, 401s once; exactly two verify | one probe result applied to a whole backend instead of per binding |
| `TestUnverifiedOllamaBindingIsNotCallable` — excluded by `inferenceBindingCallable`, `callableRuntimeModels`, and the compiled manifest | reverting `bindingNeedsHostProof` to the `Auth != "1password"` shortcut (`inference.go:611`) — the hole that makes honest verification cosmetic |
| `TestNilOllamaProbeSeamLeavesBindingsUnverified` | a test-mode default that fabricates success; a real call leaking out of a hermetic test |
| `TestVerifyOllamaInferenceUsesResolvedEndpoint` — `OLLAMA_HOST=10.0.0.5:11500` reaches the prober intact | a hardcoded loopback; pairs with `check-endpoint-literals.sh --self-test` |
| `TestLocalOllamaProbesAreSerialized` — a fake prober records overlap; max concurrent local probes == 1 while two cloud probes DO overlap | **B1**: re-introducing a shared errgroup across the whole binding set, which co-loads local weights |
| `TestLocalProbeSendsKeepAliveZeroAndRungContext` — the request body carries `keep_alive: 0` and `num_ctx` == the rung's `context_window` | dropping the unload (probe *n+1* stacks on probe *n*'s weights) or probing at a context the gate never priced |
| `TestLocalProbeOrderIsLargestRungFirst` | spending the local budget on the 4b and never reaching the rung the roster will use |
| `TestLocalProbeBudgetMarksRemainderNotProbedNotFailed` — budget exhausted → distinct reason, excluded from `attempted`, no failure text | **B1**'s second half: a healthy model reported as broken, and a decline turned into a non-zero exit by a budget |
| `TestVerifiedBindingRecordsProbeProvenance` — promotion sets `VerifiedBy == "probe"`; demotion clears it and `VerifiedAt` | **B2**: provenance that outlives the claim, which makes the migration row lie in the other direction |
| `TestLegacyVerifiedOllamaBindingFlaggedOnceThenClears` — pre-upgrade config → row present; after a setup that re-probes → row gone (both on promote and on demote) | **B2**: the unimplementable-as-specified TODO — a row that fires forever or never |
| `TestUnverifiedOllamaCandidateRemediatesWithPullNotProviderKey` — pure-ollama declined-pull box: core check todo is `pix setup --pull-models`, and the string `ANTHROPIC_API_KEY` appears nowhere | **S3**: the fall-through to `modelKeyCoreCheck` (`doctor_providers.go:119`) |
| `TestRunIntentRowNamesThePullForUnverifiedOllamaBinding` | **S3**'s second caller (`doctor_providers.go:126`) silently stopping matching |
| `TestSynthesizeInferenceKitErrorNamesTheFix` — the `run` refusal string contains `pix setup --pull-models` | **S5**: a dead-end refusal at `inference.go:673` |
| `TestNonInteractiveModelsFlagRejectsUnprobedModel` — `--models ollama/qwen3.5:27b` with a failed probe errors, and the message names the probe | **N1**: an intended contract change landing unnoticed, or landing with the generic "not available through the selected runtime" text |
| `TestSetupMutationOrderEndsWithInference` (extends the AC-P0-303 order test) | **N2**: adding the step without pinning its position, or leaving `models` believed-last |
| `TestInferenceStepPrintsNothingOnFullSuccess` — a fully-verified run writes no "N verified" line from the mutation | **N2**/AC-P0-302: a success claim rendered by a mutation instead of by the post-mutation probe |
| `TestPullPromptNamesTheBridgeAsRequiredOnPureLocalBox` — the header does not contain "optional" when the rung is the only inference model | **N3**: the static `setup_models.go:153` string surviving the change |
| `TestCompiledRoutingVersionIsUnchanged` — asserts `CompiledRoutingVersion == 1` **and** that `extensions/subagents.ts`'s `ROUTING_SCHEMA` literal matches it | **N4**: the tidy-minded bump that bricks routing in every already-built image |
| `TestPackDeclaredOllamaBindingStaysCallableWithoutHostProof` | the pack regression this rule could cause; pairs with `TestConfigureModelRosterPreservesPersonalChoiceUnderExclusivePack` (`inference_test.go:112`) |
| `TestUsableMemoryByOS` — darwin ≤ 36 GB → 0.67, darwin > 36 GB → 0.75, linux → 0.60, other → not OK | applying the macOS fraction to Linux; collapsing the darwin tiers back to one number |
| `TestChooseLocalRungByRAM` — the corrected D2 table verbatim, including **both** 8 GB rows → none, 32 GB darwin → `9b` (not `27b`), 36 GB darwin → `27b` | an off-by-one in `FitsMemory`; a ladder sorted smallest-first; a silent revert to the pre-S1 arithmetic |
| `TestMinRAMIncludesContextBudget` — each shipped rung's `min_ram_gb` equals `ceil(download_gb*1.15 + context_window*kv_gb_per_token + 1)` | **S1**: a rung added with the old weights-only formula, or a `context_window` raised without repricing the gate |
| `TestDarwinUsableFractionIsTiered` — 32 GB → 0.67, 48 GB → 0.75 | a single 0.75 that over-promises against the macOS wired-memory limit on small machines |
| `TestLocalRungContextWindowIsBudgetedNot256K` — no `local: true` catalog entry declares a context larger than the one its `min_ram_gb` priced | **S1**'s other half: the catalog advertising a context the machine cannot hold, which `compileInferenceRuntime` then ships to the sandbox |
| `TestUnknownMemoryOffersFloorRungOnly` | "unknown means unconstrained", i.e. offering the 35B to a machine it could not measure |
| `TestModelWithoutMinRAMNeverFits` | a local catalog entry added without a gate |
| `TestHardwareCheckIsNeverReady` — every shape renders a note | invariant **13**, and `readiness_ollama.go`'s "inference is not a probe" rule |
| `TestOllamaLocalWithNoCatalogModelPulledSucceeds` — empty listing, no error, `WantPull` + `OllamaBridgeModel` set | resurrecting the hard error at `inference.go:209` |
| `TestLocalRungPullFlowsThroughExistingConsent` — `--yes` pulls nothing, `--pull-models` pulls, default-No pulls nothing | a second consent mechanism; `--yes` read as download consent |
| `TestDeclinedPullLeavesNoCallableModelAndExitsZero` — `✗ inference`, `Core not ready`, exit 0 | turning a user decision into a failure; printing ✓ for an empty inference surface |
| `TestOllamaCloudSelectedWithZeroVerifiedFailsSetup` | a silent "configured" for an account that can call nothing |
| `TestResolveRelaxesProviderBeforeAccuracy` — `Relaxed == ["provider"]` | the all-at-once cliff; a ladder in the wrong order |
| `TestBreadthOnOllamaOnlyBoxRespectsLatencyCeiling` | today's step 4 exactly: cost tie at $0 → accuracy tiebreak → the biggest local model for a fanout |
| `TestRelaxedRouteIsVisibleInReasonAndCompiledOutput` | a silently degraded route — what made the original incident invisible |
| `TestExplainDecisionNamesRelaxationNotFallback` | reverting `agent.go:229` to the ambiguous `-> fallback` string |
| `TestEveryAvailableModelIsFullyScored` (`catalog_test.go:78`, existing) | ships a rung the resolver silently drops; fails until all 12 scorecard rows land |
| `TestShippedOllamaIDsAreValidOllamaReferences` (`catalog_test.go:39`, existing) | an unmatchable id (two colons) — the future `-mlx` experiment goes through it |
| `TestShippedLocalModelsDeclareMinRAM` (new) | a rung added without a gate or a size for the consent prompt |

## Migration and compatibility

**Existing configs with `verified = true` ollama bindings** hold claims, not
evidence. They are **grandfathered as callable** until the next `pix setup`, and
`pix doctor` prints one TODO row. The predicate is exact, and it is only
expressible because of the `VerifiedBy` field added above (**B2**):

```go
// legacyVerifiedOllamaBindings returns bindings that claim Verified without
// naming a probe. Only a pre-upgrade config can produce this shape: everything
// this codebase promotes writes VerifiedBy="probe" in the same assignment.
func legacyVerifiedOllamaBindings(cfg *config.Config) []config.InferenceModelBinding {
	// b.Verified && b.VerifiedBy != "probe" && backend driver == "ollama"
}
```

```
  ⚠ inference    3 ollama binding(s) were marked verified by a listing, not a request — re-verify: pix setup
```

The row **clears correctly**, which is the property rev 1 could not deliver: the
next `pix setup` runs `verifyOllamaInference`, which either promotes the binding
with `VerifiedBy = "probe"` (predicate false, row gone) or demotes it to
`Verified: false` (predicate false, row gone, replaced by the S3 candidate row
that names the real problem). There is no state in which it false-flags forever
and none in which it silently never fires. `callableRuntimeModels` keeps
grandfathering on `Verified` alone, so provenance never affects callability — it
only affects what doctor *says*.

Demoting them at load would empty `callableRuntimeModels` on a working local box
and `pix run` would refuse to launch. That is a false refusal produced by a
bookkeeping change rather than by evidence — the same failure invariant **6**
guards against on the key path, though (per **S5**) not the same mechanism.
Setup is the transaction that re-verifies, and it is what a user runs after an
upgrade anyway. *Rejected:* a hard demote at load, and a silent re-verification
inside `run` (a multi-second cold-load probe in the launch path).

**Prompt-answer compatibility — token `2` narrows (review finding S4).** Rev 1
claimed existing answers keep their meaning. That holds for `1` and `3`; it is
**false for `2`**, and the falsity is the point of the fix. Today the bind loop
filters only on `m.Provider == "ollama" && m.Available` (`inference.go:196-207`),
so answering `2` binds **every** listed catalog ollama model — `:cloud` tags
included. After D1, `2` means *Ollama local* and binds no cloud model at all. A
user (or a script) who answered `2` and got cloud bindings must now answer
`2,4`. This is a deliberate narrowing of an over-broad token — the un-asked-for
cloud binding is the kimi-k3 delivery mechanism — but it is a behavior change to
a stable answer and it is documented here, in the release note, and in the
menu's own wording (`2. Ollama local`, `4. Ollama Cloud`). The blast radius is
bounded by D4: a silently dropped cloud binding could not have been callable
without passing a probe anyway, so the worst case is a user who must re-run
setup and answer `2,4`, not a user whose working setup starts failing at call
time.

**Pack compatibility.** Three preserved properties, each with a test:
`setupChooseInference`'s early return when `len(cfg.Inference.Backends) > 0`
(`inference.go:52-60`) is untouched, so a pack-configured host never sees the new
prompt; `bindingNeedsHostProof` exempts `Source != ""`, so a pack-declared
ollama backend stays callable with `Verified: false` exactly as
`enableDeclaredInferenceBindings` (`inference.go:456`) intends; and
`ExclusiveSource`/`ExclusiveBackend` filtering (`inference.go:588-604`) is
untouched.

**Config compatibility.** No new config keys. `min_ram_gb` and `download_gb` are
optional JSON fields in the shipped catalog, so a user's
`~/.pix/routing/models.json` override that predates them still parses — and
`TestModelWithoutMinRAMNeverFits` makes the degradation conservative (an
override's local models are not offered until it declares the gate).

**`routing.json` compatibility.** `Relaxed` is additive and optional;
`subagents.ts` reads `routes[*].model` and ignores unknown fields, so no image
rebuild is required for old sandboxes to keep working — **provided
`CompiledRoutingVersion` stays `1`.** That condition is doing all the work here:
`subagents.ts:154` rejects any version that is not an exact match and drops every
agent to "inherit parent model". See the boxed rule in the data-model section
(**N4**); it is the single easiest way to turn this change into a fleet-wide
regression. Picking up the new reasons needs `pix route compile` (and `make
load` for a baked image), which is the normal routing workflow.

**Invariants touched, by number:**
**13** (success words earned by a probe) — the point of D4, the rule
`hardwareCheck` obeys in D2, and why the new step prints no routine "N verified"
line. **6** (`run` refuses only on a *positive* `sbx secret ls` answer) — **not
the mechanism that fires in the declined-pull state** (see S5 in D5): that
refusal comes from `synthesizeInferenceKit` (`inference.go:673`) reading our own
config. What this design owes invariant 6 is its *spirit*, and it pays it twice:
by grandfathering legacy bindings instead of demoting them at load, and by
keeping "not probed" distinct from "failed" so a budget-exhausted candidate is
never mistaken for a proven-bad one. **1** (config managed by `pix config set`)
— the local rung is written through the config API into the existing
`ollama_bridge_model` key, and the new `verified_by`/`verified_at` fields are
written by `cfg.Save()`'s sparse encode, never hand-edited. **8** (pack trust) —
untouched; the `Source` exemption is what keeps it that way.

## Open questions

1. **`-mlx` variants on Apple Silicon.** `9b-mlx` (8.9 GB), `27b-mlx` (20 GB),
   `35b-mlx` (22 GB) are meaningfully faster on Apple Silicon. Carrying them
   doubles the local scorecard for a latency delta, and the tag is arch-specific
   so the gate would need an arch dimension. Proposal: leave out of v1; revisit
   once `chooseLocalRung` has an arch input.
2. **The 122B rung.** Under the corrected S1 arithmetic the knife-edge is gone,
   and it went the other way: `ceil(81 × 1.15 + 32768 × 192 KB + 1)` ≈
   `ceil(93.2 + 6.0 + 1)` = **101 GB usable**, against 96 GB usable on a 128 GB
   Mac at 0.75. It does **not** fit the largest machine we ship for, on top of an
   81 GB download. Not carried, and no longer a close call — which also retires
   rev 1's question about raising the darwin fraction to 0.80 to reach it
   (raising a fraction to make a model fit is fitting the ruler to the answer).
   Remaining question: is a manual `pix config set ollama_bridge_model
   qwen3.5:122b` on a 192 GB Mac Studio worth a documented escape hatch, or does
   `FitsMemory` correctly refuse to route what it cannot size?
3. **Discrete-GPU Linux.** The 0.60 fraction is a proxy for "we do not probe
   VRAM", and it under-serves a 24 GB RTX box badly (32 GB system RAM → 19.2
   usable → the 9B, while 24 GB of VRAM would hold the 27B comfortably).
   Worth an `nvidia-smi --query-gpu=memory.total` reading behind a capability
   probe, or is that the start of a hardware-detection swamp?
7. **The darwin tier boundary.** 36 GB is the documented-ish inflection for the
   default `iogpu.wired_limit_mb`, but Apple has never contracted it and it has
   moved across OS releases. Options: keep the two-tier constant and re-ground it
   the way `model-refresh` re-grounds the scorecard; read the sysctl directly
   when it is non-zero (a user who raised it has told us their budget) and fall
   back to the tier otherwise; or collapse to a flat 0.67 and under-serve big
   machines by one rung. Leaning toward reading the sysctl when set — it is the
   only source that is not a guess — but it is a second seam, so: v2.
8. **`ctx_budget_tokens` vs how pi actually works.** 32K on the top rungs is
   honest about RAM and *short* for the sessions this product runs (a coding
   session routinely passes 100K). The consequence is that a local rung is a good
   subagent/bridge model and a poor top-level session model, which the router
   already reflects through latency scoring. Should the offer text say that out
   loud ("local models are sized for subagents and the bridge; the top-level
   session wants a cloud model") rather than leaving the user to discover it by
   filling the window?
4. **Discovering cloud tags from the listing instead of the catalog.** Ollama
   ships cloud models faster than we re-ground the catalog. Binding any listed
   `:cloud` tag and letting D4's probe decide is tempting — but an unscored
   model cannot be routed (`Resolve` skips models with no scorecard row), so it
   would need a default score, which is a guess by another name. Leaning no.
5. **Should the roster prompt be skipped when only one local rung is callable?**
   `configureModelRoster` already auto-selects a single candidate
   (`inference.go:270-273`), so the common local-only case is silent — but a
   local + cloud user gets a menu of 6. Is that the right amount of choice?
6. **Flow 3 (Ollama as a keyed reseller).** Explicitly out of scope. When we
   want keyed access to third-party models, it should arrive as an OpenRouter
   backend with its own credential story, not as a third Ollama mode.
