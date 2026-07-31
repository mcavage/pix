#!/usr/bin/env node
// U-W0b.13 (AC-P0-111, AC-P0-112) — what this repo puts into EVERY turn.
//
// Always-on prompt content is the one cost paid on every request of every
// session, and until it is attributed nobody can tell a 15 KB reference block
// from a 200-byte tool snippet. This measures each segment, says who owns it,
// and marks which ones this repository can actually do something about.
//
// GATED vs REPORTED, and why the split is not a dodge:
//   * PROJECT-OWNED segments are gated. This repo writes every byte, so a
//     budget is a real control.
//   * HOST-OWNED segments are reported with attribution and NOT gated: an
//     ancestor AGENTS.md on the user's machine is ~23 KB that this repository
//     cannot rewrite, and pi's own tool schemas move when pi moves. Gating on a
//     number you do not control produces a waiver, and a waived gate teaches
//     that gates are negotiable.
//
// Usage:
//   node scripts/measure-context.mjs            human table
//   node scripts/measure-context.mjs --json     machine-readable
//   node scripts/measure-context.mjs --check    exit 1 if a gated budget is over
//   node scripts/measure-context.mjs --ancestor <file>   attribute a real
//                                               host-owned ancestor AGENTS.md
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const KB = 1024;

/**
 * AC-P0-111. Project-owned always-on content is capped at 18 KB total, split
 * across three sub-budgets. These are the PRD's numbers, written here once.
 */
export const BUDGETS = {
	"project-context": 8 * KB,
	"skill-catalog": 8 * KB,
	"extension-snippets": 2 * KB,
	"project-owned-total": 18 * KB,
};

const repoRootFrom = (dir) => path.resolve(dir);

const bytes = (s) => Buffer.byteLength(s ?? "", "utf8");
const readIf = (p) => (fs.existsSync(p) ? fs.readFileSync(p, "utf8") : null);

/**
 * The project context files this repo owns and ships as always-on prompt text:
 * the root AGENTS.md, plus the sandbox kit's `agentInstructions.content` (sbx hands it to
 * the agent at launch, so it is on every turn just like AGENTS.md).
 */
function projectContext(root) {
	const parts = [];
	const agents = readIf(path.join(root, "AGENTS.md"));
	if (agents !== null) parts.push({ name: "AGENTS.md", bytes: bytes(agents) });

	const spec = readIf(path.join(root, "pi-kit/spec.yaml"));
	if (spec !== null) {
		const marker = "  content: |\n";
		const i = spec.indexOf(marker);
		if (i !== -1) {
			// Count the instructions sbx injects, not the four YAML indentation
			// bytes on every source line.
			const content = spec
				.slice(i + marker.length)
				.split("\n")
				.map((line) => (line.startsWith("    ") ? line.slice(4) : line))
				.join("\n");
			parts.push({ name: "pi-kit/spec.yaml agentInstructions", bytes: bytes(content) });
		}
	}
	return parts;
}

/**
 * The skill catalog: pi injects one entry per discovered skill (name,
 * description, location) on every turn, while skill BODIES stay on demand.
 *
 * Two numbers, and the difference matters. `bytes` counts only what this repo
 * authors — the `name` and `description` in each SKILL.md's frontmatter, which
 * is the thing a budget can actually move. `formattedBytes` adds pi's XML
 * wrapper and the absolute install path it computes, which this repo does not
 * write and cannot shorten; it is reported so the total is honest.
 */
function skillCatalog(root) {
	const dir = path.join(root, "skills");
	if (!fs.existsSync(dir)) return { entries: [], bytes: 0, formattedBytes: 0 };
	const entries = [];
	let authored = 0;
	let formatted = 0;
	for (const name of fs.readdirSync(dir).sort()) {
		const file = path.join(dir, name, "SKILL.md");
		if (!fs.existsSync(file)) continue;
		const fm = /^---\n([\s\S]*?)\n---\n/.exec(fs.readFileSync(file, "utf8"));
		if (!fm) continue;
		const field = (key) => (fm[1].split("\n").find((l) => l.startsWith(`${key}:`)) ?? "").slice(key.length + 1).trim();
		const skillName = field("name") || name;
		const description = field("description");
		const own = bytes(skillName) + bytes(description);
		const location = `/home/agent/.pi/agent/skills/${name}/SKILL.md`;
		const wrapped = bytes(
			`  <skill>\n    <name>${skillName}</name>\n    <description>${description}</description>\n    <location>${location}</location>\n  </skill>\n`,
		);
		entries.push({ skill: name, bytes: own, formattedBytes: wrapped });
		authored += own;
		formatted += wrapped;
	}
	return { entries, bytes: authored, formattedBytes: formatted };
}

/**
 * Extension prompt real estate: `promptSnippet` (one line per tool, always in
 * the prompt) and `promptGuidelines` (bullets pi keeps in front of the model
 * whenever the tool is active). Both are authored here, both are always-on.
 *
 * Read from source rather than by loading the extensions: loading them binds
 * ports and writes settings files, and a measurement script must not have side
 * effects. String concatenation in a snippet is summed from its literals.
 */
function extensionSnippets(root) {
	const dir = path.join(root, "extensions");
	if (!fs.existsSync(dir)) return { entries: [], bytes: 0 };
	const entries = [];
	let total = 0;
	for (const file of fs.readdirSync(dir).filter((f) => f.endsWith(".ts")).sort()) {
		const src = fs.readFileSync(path.join(dir, file), "utf8");
		let own = 0;
		for (const m of src.matchAll(/\bpromptSnippet:\s*("(?:[^"\\]|\\.)*")/g)) own += bytes(JSON.parse(m[1]));
		for (const m of src.matchAll(/\bpromptGuidelines:\s*\[([\s\S]*?)\n\t*\]/g)) {
			for (const lit of m[1].matchAll(/"(?:[^"\\]|\\.)*"/g)) own += bytes(JSON.parse(lit[0]));
		}
		// Guideline bodies pulled out into a named const (the common shape once a
		// string is shared between two tools) are counted where they are declared.
		for (const m of src.matchAll(/^const [A-Z0-9_]+(?:_SEMANTICS|_GUIDELINE) =\s*([\s\S]*?);\n/gm)) {
			for (const lit of m[1].matchAll(/"(?:[^"\\]|\\.)*"/g)) own += bytes(JSON.parse(lit[0]));
		}
		if (own) {
			entries.push({ extension: file, bytes: own });
			total += own;
		}
	}
	return { entries, bytes: total };
}

/**
 * Net-new recall bytes per user turn: the ceiling the transport enforces, not
 * an observation. Two independent channels (memory :11435, knowledge :11436),
 * each capped by lib/recall-message.ts.
 */
function recallCeiling(root) {
	const src = readIf(path.join(root, "lib/recall-message.ts")) ?? "";
	const m = /RECALL_BYTE_CAP\s*=\s*(\d+)/.exec(src);
	const cap = m ? Number(m[1]) : 0;
	return { perChannel: cap, channels: 2, bytes: cap * 2 };
}

/** Everything, attributed. */
export function measureContext(root = repoRootFrom(process.cwd()), opts = {}) {
	const project = projectContext(root);
	const skills = skillCatalog(root);
	const ext = extensionSnippets(root);
	const recall = recallCeiling(root);

	const segments = [
		{
			id: "project-context",
			label: "project context (AGENTS.md + kit agentInstructions)",
			owner: "project",
			gated: true,
			bytes: project.reduce((n, p) => n + p.bytes, 0),
			detail: project,
		},
		{
			id: "skill-catalog",
			label: `skill catalog (${skills.entries.length} entries, authored name+description)`,
			owner: "project",
			gated: true,
			bytes: skills.bytes,
			reportedBytes: skills.formattedBytes,
			detail: skills.entries,
		},
		{
			id: "extension-snippets",
			label: "extension prompt snippets + guidelines",
			owner: "project",
			gated: true,
			bytes: ext.bytes,
			detail: ext.entries,
		},
		{
			id: "skill-catalog-formatting",
			label: "skill catalog, pi's XML wrapper + install paths",
			owner: "pi",
			gated: false,
			bytes: skills.formattedBytes - skills.bytes,
		},
		{
			id: "recall-net-new",
			label: `recall net-new per turn (ceiling: ${recall.channels} channels x ${recall.perChannel} B)`,
			owner: "project",
			gated: false, // enforced by lib/recall-message.ts and its own tests, not by a byte budget here
			bytes: recall.bytes,
		},
	];

	// The host-owned ancestor AGENTS.md. Reported when the caller points at a
	// real one; otherwise recorded as unmeasured rather than as zero, because
	// "zero" would read as "no ancestor context" when it means "not looked at".
	const ancestorPath = opts.ancestor ?? process.env.PIX_ANCESTOR_AGENTS ?? null;
	const ancestorText = ancestorPath ? readIf(ancestorPath) : null;
	segments.push({
		id: "ancestor-context",
		label: ancestorPath ? `ancestor AGENTS.md (${ancestorPath})` : "ancestor AGENTS.md (not measured on this host)",
		owner: "host",
		gated: false,
		measured: ancestorText !== null,
		bytes: ancestorText === null ? 0 : bytes(ancestorText),
	});

	const projectOwnedGated = segments.filter((s) => s.gated);
	const projectOwnedTotal = projectOwnedGated.reduce((n, s) => n + s.bytes, 0);
	const coldTurnTotal = segments.reduce((n, s) => n + s.bytes, 0);

	const budgets = projectOwnedGated.map((s) => ({
		id: s.id,
		bytes: s.bytes,
		budget: BUDGETS[s.id],
		over: s.bytes > BUDGETS[s.id],
	}));
	budgets.push({
		id: "project-owned-total",
		bytes: projectOwnedTotal,
		budget: BUDGETS["project-owned-total"],
		over: projectOwnedTotal > BUDGETS["project-owned-total"],
	});

	return { segments, budgets, projectOwnedTotal, coldTurnTotal };
}

// ── CLI ──────────────────────────────────────────────────────────────────────

function fmt(n) {
	return `${String(n).padStart(7)} B${n >= KB ? ` (${(n / KB).toFixed(1)} KB)`.padStart(11) : "".padStart(11)}`;
}

function main(argv) {
	const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
	const ai = argv.indexOf("--ancestor");
	const result = measureContext(root, { ancestor: ai === -1 ? undefined : argv[ai + 1] });

	if (argv.includes("--json")) {
		process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
	} else {
		console.log("always-on prompt content, by segment\n");
		console.log(`  ${"segment".padEnd(52)} ${"bytes".padStart(20)}  owner   budget`);
		console.log(`  ${"-".repeat(52)} ${"-".repeat(20)}  ------  ------`);
		for (const s of result.segments) {
			const b = result.budgets.find((x) => x.id === s.id);
			const verdict = !s.gated ? "reported" : b.over ? `OVER by ${b.bytes - b.budget} B` : `ok (<= ${b.budget} B)`;
			console.log(`  ${s.label.padEnd(52)} ${fmt(s.bytes)}  ${s.owner.padEnd(6)}  ${verdict}`);
			for (const d of s.detail ?? []) {
				if (d.bytes < 512 && s.id === "skill-catalog") continue; // per-skill noise
				console.log(`      ${(d.name ?? d.skill ?? d.extension).padEnd(48)} ${fmt(d.bytes)}`);
			}
		}
		const total = result.budgets.at(-1);
		console.log("");
		console.log(`  project-owned always-on total: ${total.bytes} B (budget ${total.budget} B) ${total.over ? "OVER" : "ok"}`);
		console.log(`  cold turn (this repo's view, NOT gated): ${result.coldTurnTotal} B`);
	}

	if (argv.includes("--check")) {
		const over = result.budgets.filter((b) => b.over);
		if (over.length) {
			for (const b of over) console.error(`over budget: ${b.id} ${b.bytes} B > ${b.budget} B`);
			return 1;
		}
	}
	return 0;
}

if (import.meta.url === `file://${process.argv[1]}`) process.exit(main(process.argv.slice(2)));
