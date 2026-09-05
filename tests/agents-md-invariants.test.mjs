// U-W0b.08 (AC-P0-114), carried forward into Pix v2 — AGENTS.md is always-on
// prompt content, so every byte in it is paid for on every turn of every
// session. It must not carry a hand-copied verb/flag catalogue (that lives in
// `pix help --all` and docs/reference.md, both generated or maintained closer
// to the code) and it must not silently drop a load-bearing safety property.
//
// The Pix v2 cutover (docs/design/pix-v2-architecture.md, docs/design/
// pix-v2-surface.md) replaced the whole command surface and deleted
// pix-host/packs/routing/XDG-split state. The invariant set below was
// rewritten to match: it enumerates the v2 safety properties AGENTS.md must
// still state, not the v1 ones (config.toml via `pix config set`, pack trust,
// serve-stop-through-supervisor, ...) that described a system this cutover
// removed.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const agentsPath = path.join(repoRoot, "AGENTS.md");
const agents = fs.readFileSync(agentsPath, "utf8");

// The ceiling this trim bought, plus headroom for ordinary editing. Not
// AC-P0-111's 8 KB project-context target; a non-regression ceiling so a
// deleted reference cannot quietly grow back.
const AGENTS_MD_CEILING_BYTES = 40 * 1024;

// Each entry: the invariant in one line, and the phrases that must all still
// appear in AGENTS.md for it to count as stated. Phrases are matched
// case-insensitively so a re-word of the surrounding sentence does not fail
// the build, but deleting the fact does.
const SAFETY_INVARIANTS = [
	{
		id: "pix-home-no-xdg-split",
		invariant: "PIX_HOME is the single root; there is no XDG config/data/state/cache split.",
		phrases: ["PIX_HOME is the single root", "no xdg", "$PIX_HOME"],
	},
	{
		id: "config-single-named-writers",
		invariant: "config.toml and secrets.env each have one named writer; there is no generic config mutation command.",
		phrases: ["config.toml", "secrets.env", "one named writer", "no generic config mutation command"],
	},
	{
		id: "implicit-launch-needs-a-tty",
		invariant: "An implicit launch (bare `pix`) requires an interactive terminal; non-interactive stdin never creates or attaches a sandbox.",
		phrases: ["implicit launch requires a tty", "non-interactive stdin", "never creates or attaches"],
	},
	{
		id: "existing-sandbox-never-forced",
		invariant: "An existing sandbox is never force-removed or replayed into; unknown sbx state fails closed; --force never widens the pix-* namespace.",
		phrases: ["never force-removed or replayed into", "fails closed", "never widens the `pix-*` namespace"],
	},
	{
		id: "rm-scoped-to-pix",
		invariant: "`pix rm` only ever removes pix-* sandboxes it created.",
		phrases: ["scoped to `pix-*` sandboxes only"],
	},
	{
		id: "liveness-is-a-reference-lock",
		invariant: "Liveness is proven by a reference lock bound to the recorded sbx instance ID, never a bare PID.",
		phrases: ["reference lock bound to the recorded sbx instance", "never by a bare pid"],
	},
	{
		id: "orphans-need-five-positive-proofs",
		invariant: "`pix rm --orphans` requires five positive proofs (fresh listing, pix-* name, matching instance ID, zero reference locks, no keep marker); an unknown answer preserves the sandbox.",
		phrases: ["five positive proofs", "any unknown answer preserves the sandbox"],
	},
	{
		id: "direct-keys-1password-only",
		invariant: "Direct provider keys come from 1Password only; keyless and Gateway-authenticated backends never trigger an irrelevant 1Password flow.",
		phrases: ["direct provider keys come from 1password only", "never trigger an irrelevant 1password flow"],
	},
	{
		id: "trust-hmac-outside-environment",
		invariant: "Environment trust is HMAC-bound and stored outside the environment; a changed fingerprint refuses launch; --yes never skips the fingerprint check.",
		phrases: ["hmac-bound", "stored outside the environment", "it never skips the fingerprint check", "trust review defaults to no"],
	},
	{
		id: "pix-host-packs-routing-deleted",
		invariant: "pix-host, packs, scored model routing, and the custom memory RPC are deleted outright, not merely hidden; no code path reaches any of them.",
		phrases: ["deleted, not merely hidden", "no code path reaches any of them", "no unsandboxed host-agent mode"],
	},
	{
		id: "memory-is-mcp-tools-only",
		invariant: "Memory is operated only through MCP tools (memory_recall/remember/forget/observe/stats/status/snapshot/restore); there is no private memory protocol.",
		phrases: ["memory_recall", "memory_remember", "never a private protocol"],
	},
	{
		id: "success-words-are-probed",
		invariant: "`ready`/`verified` are post-probe success words; doctor never repairs, registers, restarts, or authenticates, and never prints `configured`/`enabled` as a verdict.",
		phrases: ["success words are earned by a probe", "never repairs, registers, restarts, or authenticates"],
	},
];

// Markdown line-wraps a long sentence across lines; a phrase that happens to
// straddle a wrap point should not fail this check over whitespace alone, so
// both the haystack and the needle are compared with runs of whitespace
// (including newlines) collapsed to one space.
const collapse = (s) => s.replace(/\s+/g, " ").toLowerCase();
const agentsFlat = collapse(agents);

test("every enumerated safety invariant is still stated in AGENTS.md", () => {
	const missing = [];
	for (const { id, invariant, phrases } of SAFETY_INVARIANTS) {
		const gone = phrases.filter((p) => !agentsFlat.includes(collapse(p)));
		if (gone.length) missing.push(`${id}: ${invariant}\n    missing phrase(s): ${gone.map((p) => JSON.stringify(p)).join(", ")}`);
	}
	assert.deepEqual(missing, [], `AGENTS.md dropped safety invariant(s):\n  ${missing.join("\n  ")}`);
});

test("the invariant enumeration itself is not silently shrunk", () => {
	// Deleting a row from SAFETY_INVARIANTS is the easy way to make the test
	// above pass while losing the invariant. Removing one now has to also edit
	// this number, which shows up in review.
	assert.equal(SAFETY_INVARIANTS.length, 12);
	assert.equal(new Set(SAFETY_INVARIANTS.map((i) => i.id)).size, SAFETY_INVARIANTS.length);
});

test("AGENTS.md carries no CLI reference block", () => {
	const launcher = section(agents, "## Command surface");
	assert.ok(launcher, "AGENTS.md must keep a `## Command surface` section (the pointers live there)");

	// The deleted block was a verb catalogue: one bulleted entry per verb, each
	// leading with a `pix <verb> …` code span. Three or more of those in one
	// section means the reference is growing back.
	const verbBullets = launcher.split("\n").filter((l) => /^\s*[-*]\s+\*{0,2}`pix [a-z]/.test(l));
	assert.ok(
		verbBullets.length < 3,
		`the command-surface section is turning back into a verb reference (${verbBullets.length} verb bullets); it belongs in \`pix help --all\` and docs/reference.md:\n${verbBullets.join("\n")}`,
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
	const launcher = section(agents, "## Command surface");
	const docPaths = [...launcher.matchAll(/`(docs\/[A-Za-z0-9._\/-]+\.md)`/g)].map((m) => m[1]);
	assert.ok(docPaths.length >= 1, "the command-surface section must point at docs/reference.md");
	const missing = docPaths.filter((p) => !fs.existsSync(path.join(repoRoot, p)));
	assert.deepEqual(missing, [], `AGENTS.md points at doc(s) that do not exist: ${missing.join(", ")}`);
	assert.ok(launcher.includes("pix help --all"), "AGENTS.md must point at the generated verb tree");
	assert.ok(docPaths.includes("docs/reference.md") || agents.includes("docs/\nreference.md"), "AGENTS.md must point at docs/reference.md");
});

test("docs/reference.md carries the command map AGENTS.md defers to, with only the v2 verb set", () => {
	const ref = fs.readFileSync(path.join(repoRoot, "docs/reference.md"), "utf8");
	const map = section(ref, "## 0. Command map");
	assert.ok(map, "docs/reference.md must have a `## 0. Command map` section");
	for (const verb of ["run", "ls", "rm", "task", "env", "secret", "setup", "doctor", "reset"]) {
		assert.ok(new RegExp(`\`${verb}[ \`]`).test(map), `command map is missing \`${verb}\``);
	}
	// Verbs the v2 cutover deleted outright must not be listed as live rows in
	// the command map (a "was removed" mention elsewhere in the doc is fine;
	// this only guards the map table itself).
	for (const verb of ["serve", "pack", "mcp", "config", "models", "agent", "uat", "resume", "status"]) {
		assert.ok(
			!new RegExp(`^\\|\\s*\`?${verb}\\b`, "m").test(map),
			`command map lists a removed verb as a live row: \`${verb}\``,
		);
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
