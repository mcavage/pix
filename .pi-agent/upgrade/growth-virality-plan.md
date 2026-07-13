# pi-stack: virality and adoption plan

_Written July 2026. Assumptions stated inline. Scope: solo-to-team adoption flywheel with Docker as the ultimate adoption target._

---

## The wedge (one sentence)

**"pi-stack is the only coding agent you can run completely full-auto because the disposable VM is the safety boundary — there is nothing on the host for it to destroy."**

### Why this is the wedge, not just a feature

Every other coding agent (Cursor, Copilot, Cline, Claude Code) runs on your host. That's why they all have approval prompts: to protect your files, your keys, your shell. The prompts are the admission that the agent could wreck something.

pi-stack doesn't have that problem. The sbx VM is isolated — network locked to an allowlist, keys never inside the VM (the proxy injects them), and you throw the whole thing away when done. There is literally nothing the agent can break that you can't discard in three seconds. That is why full-auto is safe here and nowhere else.

The cross-vendor review loop (Claude writes, GPT argues against the diff) is compelling and differentiating. But it's secondary. You could wire that up without sbx. The full-auto, zero-prompts, no-babysitting experience is ONLY available because of sbx. Anyone who tries pi-stack is adopting sbx — there is no version of the pitch that doesn't require it.

**The assume-to-validate:** this wedge assumes the ICP cares about prompt fatigue / babysitting more than raw capability. Evidence to collect: watch HN/Reddit comments on Claude Code and Cursor — the top complaints are almost always "it asked me again" or "it broke my file." If that pattern holds, this framing converts.

---

## The Docker product flywheel (sequenced)

The goal: pi-stack adoption drives sbx → Cloud MCP Gateway → hosted sandboxes. Each stage pulls the next one in naturally, without a sales motion.

### Stage 1 — Solo: pi-stack → sbx (the mandatory install)

The relationship: you cannot run pi-stack without sbx. That's structural, not incidental. The five-line quickstart (`sbx secret set -g anthropic`, `sbx run pi-stack --kit git+...`) makes sbx the first thing someone installs. `sbx secret set` also becomes the API key manager — it's the credential store for Claude, OpenAI, Gemini, and GitHub from minute one.

**Docker product activated:** `sbx` runtime + Docker Desktop.

**What makes someone install it:** the demo GIF showing `ship` running end-to-end — tests pass, a GPT subagent argues against Claude's diff, PR opens — with zero prompts. That 12-second recording is the whole pitch. The share moment is "I watched it open a PR while I was in another window." That story only travels if the GIF exists and is the README hero.

### Stage 2 — Depth: more tools → Cloud MCP Gateway

Once someone runs pi-stack daily, they want their existing tools in the loop: Slack, Notion, Linear, Jira. The path to those is `sbx mcp add` + the Cloud MCP Gateway. The gateway is already wired in `mcp.json`; the user just needs to register their tools.

`make mcp-register` is the activation event. At that point, the gateway is load-bearing — skills like `investigate` can pull an issue from Linear, file a Slack summary, and commit, all in one task. That capability is addictive and shares well ("show me the Slack thread and I'll fix the bug" as a single prompt).

OKF knowledge bundles, when they land, are the team-level version: shared context injected automatically means every sandbox on the team has the same institutional knowledge. That's a Cloud MCP Gateway / hosted storage story naturally.

**Docker product activated:** Cloud MCP Gateway (Atlassian, Notion, Granola, Linear catalog).

**What makes them wire it:** the first time a skill says "I can't find your Linear issues — wire `mcp-register`" and they realize what's possible.

### Stage 3 — Team: shared kits + hosted sandboxes

The mixin kit format is the team-level sharing primitive. When an engineer gets pi-stack working and customizes it with skills for their stack (`--kit ./company-kit`), the natural next move is: "everyone on the team should use this." That means:

1. Publishing the mixin kit to a private GitHub repo → teammates use `--kit git+https://github.com/company/pi-kit.git#dir=kit` → each teammate installs sbx to run it
2. Running sandboxes in Docker's cloud instead of on a laptop → no local setup friction for new teammates, faster cold start, and the kit + secrets are portable
3. Team memory (OKF bundles or hosted memory service) → shared knowledge across all sandboxes → Docker's cloud storage / sync story

The upgrade path from solo to team is organic: solo user publishes a kit to share with a colleague → colleague installs sbx → both want sandboxes to have the same memory and MCP tools → hosted sandboxes solve that without "run `make serve` on everyone's laptop."

**Docker product activated:** hosted sandboxes + Docker Hub for kit distribution.

**What triggers the upgrade:** laptop constraints (can't run Ollama + a fat Docker image + VSCode at the same time) or the friction of telling a teammate "you need to clone the repo, run `make serve`, configure Ollama..."

### Flywheel summary

```
demo GIF / HN post
       ↓
engineer installs sbx to try pi-stack    ← wedge
       ↓
daily use → wants Slack/Linear/Notion    ← depth hook
       ↓
wires Cloud MCP Gateway                  ← Docker product 2
       ↓
shares mixin kit with team               ← team hook
       ↓
teammates install sbx                    ← more sbx adoption
       ↓
team wants shared memory + portability   ← Docker product 3
       ↓
hosted sandboxes + cloud storage
```

No step requires a sales call. Each pull is created by a genuine user need.

---

## 3 highest-leverage growth moves

### Move 1 — Record and ship the demo GIF (the README already stubs it)

**The asset:** a real terminal recording of `ship` running end to end in a live pi-stack sandbox. No narration, no slides — the README's own comment describes it: "Record it in a real pi-stack sandbox running `ship` end to end, zero prompts." The moment that converts: watching a GPT subagent argue against Claude's diff automatically, then a PR opening. Aim for 10-15 seconds sped up to 3x. Use asciinema → agg.

**Why this is the highest-leverage thing:** the demo GIF is literally the only missing piece between the current README and a working pitch. The words are already good. The one-command quickstart already works. Without the GIF, "no prompts, ever" is a claim. With it, it's evidence. Staff+ engineers read claims skeptically and watch GIFs immediately.

**What to capture on screen:**
1. pi running `ship` in a sandbox — one natural language prompt
2. Tests running, passing
3. The subagent tracker showing a `review` agent (different vendor) firing and returning a verdict
4. `gh pr create` output — PR URL appears
5. Zero approval dialogs anywhere

**Launch surface:** replace the README comment with the GIF, then post a Show HN: "I built a coding agent that runs full-auto because the Docker sandbox is the safety boundary." The title is the wedge. No other distribution needed initially — if the demo lands, HN will carry it.

**Assumption to validate:** the demo reads as "I want this" not "this is a toy." Validate by showing it to two staff engineers who haven't seen it before asking for the GIF — gauge whether they ask "how do I install it" or "what is this actually for."

### Move 2 — A `/getting-started` tutorial that completes the first loop in under 10 minutes

**The asset:** a guided skill (`/getting-started`) that pi runs the first time someone opens it, walking through: "Here's a real task. Type this. Watch what happens." The task should be self-contained in the sandbox (no external deps): something like "find a TODO in this repo and open a PR fixing it." The loop must complete — tests run, review fires, PR opens — before the tutorial ends.

**Why this matters:** the current README gets someone to `sbx run pi-stack --kit git+...` successfully. Then they're staring at a pi prompt with no idea what to type. The drop-off is here, not at install. A guided first loop that completes end-to-end converts lurkers into believers in one session.

**Concrete spec:**
- `/getting-started` skill auto-suggests itself on first launch (via a session counter in memory)
- Step 1: pick a real file in the working repo, find a TODO
- Step 2: run `investigate` on it
- Step 3: run `ship` — watch it run tests, trigger the cross-vendor review, open the PR
- The skill narrates what's happening at each stage ("This is the cross-vendor review — a different model is arguing against the diff")
- Ends with: "Your sandbox is done. `sbx rm` it. Nothing touched your machine."

**Launch surface:** this is a retention play, not acquisition. It pairs with Move 1: HN post drives install, tutorial drives activation. Measure activation rate as "percentage of installs that complete at least one `ship` cycle" — that's the signal that the tutorial works.

### Move 3 — Publish one polished public mixin kit as the shareable artifact

**The asset:** `pi-stack-kit-oss` — a mixin kit published to GitHub targeting open-source maintainers. Skills for: issue triage (pull GitHub issues, categorize, draft responses), automated changelog (diff since last tag → structured changelog), release notes, contributor welcome message generation. These are real recurring tasks for OSS maintainers and map perfectly to the "full-auto, no prompts" pitch.

**Why OSS maintainers as the first kit ICP:** they're staff+ engineers (hit the ICP), they work in public (kit gets visibility), their work is repetitive and well-defined (easy to write skills for), and they're already motivated to try tools that reduce maintenance burden. An OSS maintainer sharing "I use this kit to triage issues automatically" is the organic distribution that reaches exactly the right audience.

**The kit's double job:**
1. Each engineer who installs it adopts sbx (they need `sbx run pi-stack --kit ... --kit git+pi-stack-kit-oss.git`)
2. The kit itself is a concrete example of "here's how you publish a kit" — the scaffold in `examples/overlay/` already exists, and a real published kit makes that doc real

**Launch surface:** submit the kit to Docker devrel as a guest post or Docker Labs example. Docker has a vested interest in showing off the kit + MCP gateway story — pitch it as "here's a real published kit that uses the MCP gateway to pull GitHub issues." That's Docker's case study as much as it's yours.

**Assumption to validate:** OSS maintainers have enough shared workflow structure that one kit works for many of them. Test by finding 3 OSS maintainers who'll try it before publishing. If their workflows diverge too much, narrow to a single skill (issue triage only) and expand from there.

---

## Ecosystem / registry assessment

**Is "share your kit" a real viral loop or premature?**

The mixin kit format is genuinely extensible and publishable today — it's a git URL and a `spec.yaml`. No registry needed to use it. A kit shared on GitHub is already installable with `--kit git+https://...`. That part is real.

But a registry has two preconditions: supply (enough distinct kits exist) and demand (enough people looking for kits). Right now, supply is one kit (pi-stack's own) and demand is unknown because the install base is tiny. Building a registry before there are 10 external kits in the wild would be investing in distribution infrastructure for a product nobody's making yet.

**The right investment right now:**

1. A `## Community kits` section in the README — a curated list, manually maintained, links to published GitHub repos. A GitHub awesome-list works too. This is the "registry" for the first 6 months. Zero infrastructure, visible to every README reader.
2. A `docs/publish-a-kit.md` that's 1 page: fork `examples/overlay/`, add skills, push to GitHub, submit a PR to add it to the list. Make publishing a kit a 20-minute task.
3. Watch for the signal: if >5 external kits appear in 90 days, start thinking about a proper registry or Docker Hub integration. If fewer than 5, the bottleneck is install base, not discoverability — stay focused on acquisition (Moves 1 and 2) before investing in ecosystem tooling.

**The OKF bundle angle:** OKF (Open Knowledge Format / knowledge bundles) is potentially a stronger ecosystem play than kit publishing because it's content, not code. An engineer sharing "here's my team's knowledge bundle for Python microservices" is more broadly useful than a skill that only runs on their specific stack. But this depends on OKF actually landing in pi-stack — treat it as a future ecosystem lever, not a current one.

**Verdict:** kit sharing is real leverage; a registry is premature. The ecosystem motion that's worth investing in now is lowering the cost of publishing a kit (docs, scaffold) and creating visibility for what exists (README list). The flywheel kicks in when external kits exist — that's when "share your kit" becomes a growth mechanism. Until then, it's a feature, not a channel.

---

## What NOT to do

**Don't build a landing page.** Staff+ engineers who evaluate developer tools start on GitHub, not a marketing site. The README is the landing page. Invest in the README (the GIF, the quickstart clarity) not in a separate site. A landing page without substantial install numbers is vanity — it costs time and signals that the project is in "marketing mode" before it has earned users.

**Don't start a Discord.** A Discord with 30 members and 5 active conversations reads as a ghost town and actively hurts perception. GitHub Discussions is sufficient for the first few hundred users — it's indexed, it's searchable, it doesn't require joining anything. Start a Discord when there's a real reason to (e.g., people asking for a real-time space), not as a growth tactic.

**Don't track stars as a KPI.** Stars don't predict sbx installs. The metrics that matter: sbx sessions created with the pi-stack kit (Docker's metric, not yours), kit downloads (GitHub traffic), and completion of the first `ship` cycle (retention signal). If Docker will share sbx-install-from-pi-stack data, that's the metric to optimize.

**Don't try to win on features against Cursor or Claude Code.** They have more polish, more resources, and larger teams. The wedge is the thing they structurally cannot do: true full-auto with no risk to the host. Any comparison that strays into "also has web search" or "also has memory" is a losing fight. Stay on the isolation-as-safety-boundary message.

**Don't make sbx optional.** The temptation, if install friction becomes a blocker, will be to add a "run without sbx" path. That destroys the wedge. The whole value prop is that the VM IS the safety boundary — that's why you don't need prompts. A non-sbx path either needs prompts (and you've become a worse version of every other agent) or is genuinely dangerous (and you'll have an incident). The sbx requirement is load-bearing; keep it.

**Don't optimize for breadth of audience before depth of conviction.** 10 engineers who genuinely rely on pi-stack daily, talk about it, and publish kits are worth more than 1000 stars from people who tried it once. The growth goal is sbx adoption, and sbx adoption happens when engineers become daily users, not when they star a repo. Depth first, then breadth.

---

## Open assumptions for the parent

1. The "repo-less host install" redesign mentioned in scope — the plan above assumes this lands before the major push, because the current `make install` / clone-the-repo path adds friction that hurts Moves 1 and 2. If the redesign slips, the quickstart needs to explicitly acknowledge and minimize that step.
2. OKF integration timing — the team knowledge bundle story is part of the Stage 3 flywheel. If OKF lands before the team push, it strengthens the case significantly. If it slips, the team story relies on the mixin kit format alone, which is weaker.
3. Docker's willingness to co-market — Move 3 (OSS mixin kit via Docker devrel) requires Docker to be an active partner. The pitch to Docker devrel should be framed as "here's a real case study for sbx + Cloud MCP Gateway + kit publishing" — that's their story, not just yours.
4. The demo GIF conversion rate is an assumption. Before investing in the full HN launch, show the GIF to 3-5 target engineers and watch whether they say "how do I install this" or ask clarifying questions. If the latter, the demo needs a different cut.
