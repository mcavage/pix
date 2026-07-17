# Five-star audit (2026)

A full-crew audit across integrity, maintainability, correctness, completeness,
cost, security, usability, performance, and virality. Findings were produced by
parallel role subagents (security-lead, engineer, dx-consultant, devrel, docs
fanout) and verified against the tree before acting.

This branch fixes the high-value, low-risk findings. The rest are recorded below,
ranked, so they can be scheduled rather than lost.

## Fixed in this pass

**Correctness**
- `enterprise-admin` agent pinned `anthropic/sonnet-5`, which is not in the model
  registry (would fail at spawn). Reverted to `intent: reasoning`.
- **Routing redesign (the crew was a monoculture, grounded in stale data).** 13
  of 18 agents resolved to one model and the rest to Opus, and the registry was
  built from guessed model names/prices. Re-grounded the registry/scorecard on
  LIVE July 2026 vendor pricing + published benchmarks (SWE-bench, GPQA/HLE) and
  rebuilt `policy.json` into a tiered, multi-vendor crew: Fable 5 for `deep`
  (`max-accuracy`), Opus 4.8 for `strategy` (`architect`, `product-manager`),
  Sonnet 5 for `code` + the `advisory` crew, GPT-5.6 Sol for `review`, Gemini 3.1
  Pro for `red-team` (`security-lead`), Gemini 3.1 Flash-Lite for `breadth`
  (`fanout`), Haiku 4.5 for `verify` (`qa-lead`); local `gemma4:31b` as an
  offline option. New intents `strategy`/`advisory`/`red-team`; vendor diversity
  encoded via per-intent `providers` allowlists. Added the **`model-refresh`
  skill** so this refresh is a repeatable, live-data-grounded procedure, and made
  the `agent ls` **WHY column actionable** (objective + winner metrics + what it
  beat / `sole fit`).
- `pi-stack agent ls` now flags an explicit `model:` pin that is not registered
  (`pinned (UNKNOWN ...)`) instead of silently resolving to a bad model.
- `pi-stack evals --help` (launcher) advertised `--suite` and hid `import`; it now
  matches the host command (`--config`, `import`, advisory-budget note).

**Security / open-core boundary**
- Removed `SNOW_CONN` from the public `make serve` (now a generic
  overlay-populated `SERVE_ENV`).
- Removed the `snow` probe from the public `healthcheck` skill (now a generic
  `EXTRA_CLIS` hook).
- Removed the overlay-only `:11442` port from the public kit allowlist.
- Slack MCP results carrying user-authored text (message text, channel
  topics/purposes, and profile names/titles) now stamp an untrusted-content
  guard, matching gog's `--wrap-untrusted`.
- Bounded MCP Content-Length frames (`maxMCPFrameBytes`) so a hostile peer cannot
  force an unbounded host allocation.

**Docs / usability / virality**
- README leads with the outcome and a one-liner, fixes the launch command
  (`pi-stack run`, bare = status), corrects the pi link, completes the launcher
  command list, adds a "Why pi-stack?" comparison, a parallel-`task` note, and CI
  and license badges.
- AGENTS.md documents `pi-stack task` and the real `setup` wizard shape.
- Pruned five stale docs (two HANDOFFs, a superseded migration guide, a review
  artifact, an upstream draft) and added `docs/README.md` as an index.
- Added `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md`,
  and GitHub issue/PR templates.
- `make help` now points at `pi-stack help --all` for runtime/routing/agent/task.

## Deferred, ranked (recommend next)

### High

1. **Per-server MCP credential isolation.** Every host MCP server is spawned with
   the same `config/op-refs.env`, so gog, Slack, and overlay servers can read one
   another's refs (`Makefile` mcp-register, `cmd/pi-stack/mcp.go`). Move to
   per-server env files or an explicit allowlist. `--no-masking` is required for
   stdio JSON-RPC framing (masking corrupts the protocol), so keep it, but the
   shared-file exposure is the real issue.
2. **Open-core CI check fails open.** `scripts/check-open-core.sh` only blocks
   known overlay filenames and allowlisted skills/agents; a differently named
   private file or a secret in an arbitrary file passes. Add secret scanning and
   fail closed for release builds when the private marker is absent.
3. **The Make vs launcher dual control plane.** `config/local.mk` (drives
   `make run/serve/doctor`) and `~/.config/pi-stack/config.toml` (drives the
   launcher) can disagree. Pick one runtime config (the toml) and have Make
   delegate operational commands to `out/pi-stack`, keeping Make for
   build/load/install only. Rename dev-only targets `dev-run` / `dev-serve`.
4. **`run` should resume after first-run setup.** When `pi-stack run` triggers
   setup, it returns and tells the user to type `pi-stack run` again
   (`main.go` runVerb, `setup.go`). Continue the requested run after setup
   succeeds.

### Medium

5. **Eval import validation.** `ImportPromptfoo` accepts unknown model ids,
   scores outside [0,1], and negative cost/latency before `--save`
   (`routing/evals.go`). Validate against the registry and run `routing.Validate`
   before writing.
6. **`agent reassess` bakes to the wrong path.** It runs `route compile` without
   `--out`, writing `~/.pi-stack/routing/routing.json`, while Docker bakes the
   repo-root `routing.json`. Compile to `./routing.json` when run in the repo.
7. **`--budget` is advisory, not a cap.** The last model's full matrix runs whole
   (`evals.go`). Either enforce a per-batch estimate or rename to
   `--advisory-budget` (help text now says so; behavior unchanged).
8. **Pin GitHub Actions to commit SHAs.** `publish.yml` uses mutable major-version
   tags while holding Docker Hub creds and `contents: write`.
9. **op-refs file hygiene.** Registration checks existence only; require a
   `0600` regular file (no symlink) and reject literal secret values.
10. **Newline-framed MCP input has no size cap.** The Content-Length path is now
    bounded; the newline path (`ReadString('\n')`) still is not. Bound it too.

### Low

11. **Disabled fallback models pass validation.** `routing.Validate` checks a
    fallback exists but not that it is `Available`.
12. **Registry / promptfoo provider list duplication** (`defaults/models.json` vs
    `evals/promptfooconfig.yaml`) already drifted (Ollama absent from promptfoo).
    Generate providers from the registry or cross-validate.
13. **Dead code:** `Model.CostFor` is used only by its test.
14. **`pi-stack secret` naming** collides with `sbx secret` in users' heads;
    consider `pi-stack mcp credentials` with `secret` as an alias.

### Virality (product surface, not bugs)

15. Record and publish the planned 8-15s demo GIF (README has the placeholder).
16. Ship a tiny demo repo with an intentional bug + one exact prompt so the
    quickstart ends on a wow, not an empty shell.
17. Publish real autonomy numbers (time, cost, models, review findings) for a few
    representative `deliver` runs.
18. Consider moving `write-like-mark` to the overlay (it is a personal voice, and
    shipping it undercuts the "public stack is generic" claim). Replace with a
    generic voice-profile template.
19. Set GitHub topics and a social-preview image built on the one-liner.
