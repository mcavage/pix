// U-W0b.08 (AC-P0-114) — AGENTS.md is always-on prompt content, so every byte
// in it is paid for on every turn of every session. The ~15 KB launcher CLI
// reference that used to live in it duplicated `pix help --all` and
// docs/reference.md, both of which are generated or maintained closer to the
// code and therefore cannot drift the way a hand-copied verb list does.
//
// Deleting prose from a file the agent reads every turn is only safe if the
// SAFETY INVARIANTS buried in that prose survive the cut. This test is the
// enumeration: each entry below is a property that cost a real incident to
// learn, and each must remain stated in AGENTS.md. A future trim that removes
// one fails here, with the invariant named.
//
// It also pins the two halves of the deal:
//   * the CLI reference stays OUT (no verb/flag catalogue creeps back in), and
//   * the pointers AGENTS.md now offers instead actually resolve.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const agentsPath = path.join(repoRoot, "AGENTS.md");
const agents = fs.readFileSync(agentsPath, "utf8");

// The ceiling this trim bought, plus a little headroom for ordinary editing.
// It is NOT AC-P0-111's 8 KB target for project context (see
// tests/context-budget.test.mjs, which measures and reports that gap); it is a
// non-regression ceiling so the deleted reference cannot quietly grow back.
const AGENTS_MD_CEILING_BYTES = 40 * 1024;

// Each entry: the invariant in one line (this is the enumeration the PR body
// quotes), and the phrases that must all still appear in AGENTS.md for it to
// count as stated. Phrases are matched case-insensitively so a re-word of the
// surrounding sentence does not fail the build, but deleting the fact does.
const SAFETY_INVARIANTS = [
	{
		id: "config-never-hand-edit",
		invariant: "config.toml is the single runtime config, managed by `pix config set` — never hand-edited.",
		phrases: ["config.toml", "never hand-edit", "config set"],
	},
	{
		id: "implicit-launch-needs-a-tty",
		invariant:
			"An IMPLICIT launch (bare `pix`, or `pix DIR`) requires an interactive " +
			"terminal; non-interactive stdin degrades to read-only status and never " +
			"creates or attaches a sandbox.",
		phrases: ["implicit launch", "non-interactive", "read-only status"],
	},
	{
		id: "serve-stop-through-supervisor",
		invariant: "`serve stop` stops a managed daemon through its supervisor, never with a bare pid SIGTERM.",
		phrases: ["serve stop", "supervisor", "SIGTERM", "came right back"],
	},
	{
		id: "serve-state-dir",
		invariant: "Serve runtime state lives in the state dir, so `reset` cannot orphan a daemon from its pidfile.",
		phrases: ["serve.pid", "state dir", "orphan"],
	},
	{
		id: "direct-keys-1password-only",
		invariant: "Direct API keys come from 1Password only; keyless and Ollama backends never trigger that flow.",
		phrases: ["Direct provider keys come from 1Password only", "HARD failure", "never trigger an irrelevant 1Password flow"],
	},
	{
		id: "run-refuses-only-on-positive-no-key",
		invariant: "`run` refuses only on a positively confirmed missing model key; a transient probe failure proceeds.",
		phrases: ["tri-state", "transient", "false refusal"],
	},
	{
		id: "existing-sandbox-untouched",
		invariant:
			"An existing sandbox is never force-removed or replayed into; nothing recreates one implicitly (--replace is deleted); unknown sandbox state fails closed.",
		// "--replace" alone, not "`--replace` is RETIRED/DELETED": the fact is
		// that the flag is named and disclaimed, not which word disclaims it.
		// Pinning the verb made a mechanism rename (retirement -> deletion) fail
		// a build where nothing about the invariant had changed.
		phrases: ["never force-removed", "--replace", "proof-gated `pix rm BOX`", "FAILS CLOSED"],
	},
	{
		id: "pack-trust-gate",
		invariant:
			"Pack adoption that runs host code hits the Tier-1 gate; trust state is launcher-owned; unknown MCP classification fails closed; op:// creds only.",
		phrases: ["Tier-1", "non-TTY fails closed", "host-state store", "NEVER in the pack payload", "op://"],
	},
	{
		id: "host-mode-off-by-default",
		invariant: "Host mode is deleted outright (not merely off by default); `host.enabled` gates no real code path.",
		// Same loosening: "host mode" states the subject, and "host.enabled" +
		// "no code path" carry the actual claim (the flag gates nothing real).
		// The disclaiming verb is not the fact.
		phrases: ["host mode", "host.enabled", "no code path"],
	},
	{
		id: "secret-never-writes-values",
		invariant: "`pix secret` never writes a secret value to disk.",
		phrases: ["never writes a secret value to disk"],
	},
	{
		id: "rm-scoped-to-pix",
		invariant: "`pix rm` only ever removes `pix-*` sandboxes.",
		phrases: ["scoped to `pix-*` sandboxes"],
	},
	{
		id: "success-words-are-probed",
		invariant: "`ready`/`verified` are post-probe success words; `configured`/`enabled` are not success verdicts.",
		phrases: ["Success words", "post-mutation"],
	},
];

test("every enumerated safety invariant is still stated in AGENTS.md", () => {
	const missing = [];
	for (const { id, invariant, phrases } of SAFETY_INVARIANTS) {
		const gone = phrases.filter((p) => !agents.toLowerCase().includes(p.toLowerCase()));
		if (gone.length) missing.push(`${id}: ${invariant}\n    missing phrase(s): ${gone.map((p) => JSON.stringify(p)).join(", ")}`);
	}
	assert.deepEqual(missing, [], `AGENTS.md dropped safety invariant(s):\n  ${missing.join("\n  ")}`);
});

test("the invariant enumeration itself is not silently shrunk", () => {
	// Deleting a row from SAFETY_INVARIANTS is the easy way to make the test
	// above pass while losing the invariant. Removing one now has to also edit
	// this number, which shows up in review.
	//
	// 13 -> 12: "monitor ingest binds loopback by default" was retired with the
	// monitor itself. The invariant is not weakened, it is VACUOUS -- there is no
	// listener left to bind, and the :11437 network-allowlist entry that reached
	// it is gone from pi-kit/spec.yaml too.
	assert.equal(SAFETY_INVARIANTS.length, 12);
	assert.equal(new Set(SAFETY_INVARIANTS.map((i) => i.id)).size, SAFETY_INVARIANTS.length);
});

test("AGENTS.md carries no CLI reference block", () => {
	const launcher = section(agents, "## `pix` launcher");
	assert.ok(launcher, "AGENTS.md must keep a `pix` launcher section (the pointers live there)");

	// The deleted block was a verb catalogue: one bulleted entry per verb, each
	// leading with a `pix <verb> …` code span. Three or more of those in one
	// section means the reference is growing back.
	const verbBullets = launcher.split("\n").filter((l) => /^\s*[-*]\s+\*{0,2}`pix [a-z]/.test(l));
	assert.ok(
		verbBullets.length < 3,
		`the launcher section is turning back into a verb reference (${verbBullets.length} verb bullets); it belongs in \`pix help --all\` and docs/reference.md:\n${verbBullets.join("\n")}`,
	);

	// Flag tables are the other shape the reference took.
	assert.ok(!/^\|\s*(flag|verb|subverb)\b/im.test(launcher), "no flag/verb table in always-on AGENTS.md");
});

test("AGENTS.md is under its non-regression size ceiling", () => {
	const bytes = Buffer.byteLength(agents, "utf8");
	assert.ok(
		bytes <= AGENTS_MD_CEILING_BYTES,
		`AGENTS.md is ${bytes} bytes, over the ${AGENTS_MD_CEILING_BYTES}-byte ceiling. ` +
			`It is loaded into EVERY turn of every session: move reference prose into docs/ and leave a pointer.`,
	);
});

test("the pointers AGENTS.md offers instead of the reference all resolve", () => {
	const launcher = section(agents, "## `pix` launcher");
	const docPaths = [...launcher.matchAll(/`(docs\/[A-Za-z0-9._\/-]+\.md)`/g)].map((m) => m[1]);
	// `serve-lifecycle.md`-style bare names are relative to the `docs/design/` named alongside them.
	const bare = [...launcher.matchAll(/`([a-z0-9-]+\.md)`/g)].map((m) => m[1]);
	assert.ok(
		docPaths.length + bare.length >= 3,
		"the launcher section must point at docs/reference.md and the design docs",
	);
	const missing = [];
	for (const p of docPaths) if (!fs.existsSync(path.join(repoRoot, p))) missing.push(p);
	for (const b of bare) if (!fs.existsSync(path.join(repoRoot, "docs/design", b))) missing.push(`docs/design/${b}`);
	assert.deepEqual(missing, [], `AGENTS.md points at doc(s) that do not exist: ${missing.join(", ")}`);
	assert.ok(launcher.includes("pix help --all"), "AGENTS.md must point at the generated verb tree");
	assert.ok(docPaths.includes("docs/reference.md"), "AGENTS.md must point at docs/reference.md");
});

test("docs/reference.md carries the command map AGENTS.md defers to", () => {
	const ref = fs.readFileSync(path.join(repoRoot, "docs/reference.md"), "utf8");
	const map = section(ref, "## 0. Command map");
	assert.ok(map, "docs/reference.md must have a `## 0. Command map` section");
	// LIVE verbs only. `host` used to be in this list, but it only ever appeared
	// in the command map via the retired-verbs paragraph; the verb itself has
	// not existed since host mode was deleted (safety invariant 9). Requiring a
	// non-verb here made the map's own removal notice load-bearing.
	for (const verb of ["run", "status", "doctor", "setup", "serve", "pack", "mcp", "config", "task"]) {
		assert.ok(new RegExp(`\`${verb}[ \`]`).test(map), `command map is missing \`${verb}\``);
	}
});

/** The text of one `## ` section, heading included, up to the next `## `. */
function section(text, heading) {
	const start = text.indexOf(`\n${heading}`);
	if (start === -1) return "";
	const rest = text.slice(start + 1);
	const next = rest.indexOf("\n## ", heading.length);
	return next === -1 ? rest : rest.slice(0, next);
}
