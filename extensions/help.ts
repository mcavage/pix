// pi-stack — a LIVE map of the harness (`/help`) plus a warm first-run tour
// (`/getting-started`) and a one-time first-turn nudge.
//
// The whole point is that it never goes stale: `/help` enumerates the skills,
// agents, and commands that are ACTUALLY loaded, by scanning the agent dir on
// disk (skills/*/SKILL.md + agents/*.md frontmatter) and asking pi for its live
// command list. Nothing here hardcodes the full skill roster — only the GROUP
// membership lists (which bucket a skill lands in) and the reference-rule set
// (skills that auto-load as conventions and are hidden from the map) are
// constants. A skill that isn't in any group still shows up under OTHER, so a
// new skill is never silently dropped.
//
// Defensive by design: every side effect goes through `safe()`, and all output
// is via `ctx.ui.notify` — NEVER pi.sendMessage, which would trigger a fresh LLM
// turn (reasoning models 400 on assistant prefill when fired from an idle hook).

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; /* best-effort; must not break the agent */
	}
};

// Group membership: dir/skill name -> display bucket. Order here is display
// order. These are the ONLY hardcoded skill references, and only decide the
// bucket; the displayed skills always come from scanning the dirs.
const GROUPS: Array<{ title: string; members: string[] }> = [
	{ title: "WORKFLOW", members: ["brainstorm", "plan", "build", "ship", "challenge"] },
	{ title: "DEVELOP", members: ["debug", "tdd", "code-review", "peer-review", "verify"] },
	{ title: "QUALITY", members: ["qa", "design-review", "healthcheck"] },
	{
		title: "WRITE",
		members: [
			"anti-slop",
			"write-like-mark",
			"one-pager",
			"microcopy",
			"docs-sync",
			"competitive",
		],
	},
	{ title: "SYSTEM", members: ["onboard", "ingest", "promote"] },
];

// Skills that are reference rules (auto-load as conventions) — hidden from the
// main /help body, counted in a one-line footer instead.
const REFERENCE = new Set([
	"conventions",
	"api-conventions",
	"git-conventions",
	"design-system",
	"capability-routing",
	"delegation-guide",
	"guard",
	"docs-standards",
]);

// Custom + built-in commands worth surfacing even if the live command list is
// unavailable.
const KNOWN_COMMANDS = [
	"/status",
	"/subagents",
	"/recall",
	"/remember",
	"/forget",
	"/learnings",
	"/timestamps",
	"/model",
	"/reload",
	"/help",
	"/getting-started",
];

const NUDGE_MARKER = ".pi-stack-help-nudged";

// Mirror pi's getAgentDir(): $PI_CODING_AGENT_DIR or ~/.pi/agent.
function agentDir(): string {
	const os = require("node:os");
	const path = require("node:path");
	const env = (globalThis as any).process?.env ?? {};
	const override = env.PI_CODING_AGENT_DIR;
	if (override) return override.replace(/^~(?=$|\/)/, os.homedir());
	return path.join(os.homedir(), ".pi", "agent");
}

// Tiny hand-rolled YAML frontmatter parser: read between the first two `---`
// lines, split each line on its first colon. Values are trimmed and unquoted.
// Good enough for our single-line `name:`/`description:` fields; no npm dep.
function parseFrontmatter(text: string): Record<string, string> {
	const out: Record<string, string> = {};
	const lines = text.split(/\r?\n/);
	if (lines[0]?.trim() !== "---") return out;
	for (let i = 1; i < lines.length; i++) {
		const line = lines[i];
		if (line.trim() === "---") break;
		const idx = line.indexOf(":");
		if (idx === -1) continue;
		const key = line.slice(0, idx).trim();
		let val = line.slice(idx + 1).trim();
		if (
			(val.startsWith('"') && val.endsWith('"')) ||
			(val.startsWith("'") && val.endsWith("'"))
		) {
			val = val.slice(1, -1);
		}
		if (key) out[key] = val;
	}
	return out;
}

function readFrontmatter(file: string): Record<string, string> {
	const fs = require("node:fs");
	// Read just enough to cover the frontmatter block cheaply.
	const text = fs.readFileSync(file, "utf-8");
	return parseFrontmatter(text);
}

type SkillInfo = { dir: string; name: string; description: string };

function scanSkills(): SkillInfo[] {
	return (
		safe(() => {
			const fs = require("node:fs");
			const path = require("node:path");
			const dir = path.join(agentDir(), "skills");
			if (!fs.existsSync(dir)) return [];
			const out: SkillInfo[] = [];
			for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
				if (!entry.isDirectory()) continue;
				const skillFile = path.join(dir, entry.name, "SKILL.md");
				if (!fs.existsSync(skillFile)) continue;
				const fm = safe(() => readFrontmatter(skillFile)) ?? {};
				out.push({
					dir: entry.name,
					name: fm.name || entry.name,
					description: fm.description || "",
				});
			}
			return out;
		}) ?? []
	);
}

type AgentInfo = { name: string; description: string };

function scanAgents(): AgentInfo[] {
	return (
		safe(() => {
			const fs = require("node:fs");
			const path = require("node:path");
			const dir = path.join(agentDir(), "agents");
			if (!fs.existsSync(dir)) return [];
			const out: AgentInfo[] = [];
			for (const file of fs.readdirSync(dir)) {
				if (!file.endsWith(".md")) continue;
				const fm = safe(() => readFrontmatter(path.join(dir, file))) ?? {};
				out.push({
					name: file.replace(/\.md$/, ""),
					description: fm.description || "",
				});
			}
			return out;
		}) ?? []
	);
}

// Ask pi for the live command list, if the API is present. Tries ctx then pi.
function liveCommands(pi: any, ctx: any): string[] {
	const raw =
		safe(() => ctx?.getCommands?.()) ?? safe(() => pi?.getCommands?.()) ?? null;
	if (!Array.isArray(raw)) return [];
	return raw
		.map((c: any) => (typeof c === "string" ? c : c?.name))
		.filter((n: any): n is string => typeof n === "string" && n.length > 0)
		.map((n: string) => (n.startsWith("/") ? n : "/" + n));
}

// Truncate a description to a single readable clause.
function short(desc: string, max = 88): string {
	const oneLine = desc.replace(/\s+/g, " ").trim();
	if (oneLine.length <= max) return oneLine;
	return oneLine.slice(0, max - 1).trimEnd() + "…";
}

function buildHelp(pi: any, ctx: any): string {
	const skills = scanSkills();
	const agents = scanAgents();

	const byNameKey = (s: SkillInfo) => new Set([s.dir, s.name]);
	const reference = skills.filter((s) =>
		[...byNameKey(s)].some((k) => REFERENCE.has(k)),
	);
	const visible = skills.filter(
		(s) => ![...byNameKey(s)].some((k) => REFERENCE.has(k)),
	);

	// Assign each visible skill to a group; anything unmatched -> OTHER.
	const assigned = new Set<SkillInfo>();
	const L: string[] = [];
	L.push("pi-stack — what's loaded right now");
	L.push("");
	L.push("SKILLS  (run /skill:<name>, or just describe the task)");

	for (const group of GROUPS) {
		const rows: SkillInfo[] = [];
		for (const member of group.members) {
			const hit = visible.find(
				(s) => !assigned.has(s) && (s.dir === member || s.name === member),
			);
			if (hit) {
				rows.push(hit);
				assigned.add(hit);
			}
		}
		if (!rows.length) continue;
		L.push("");
		L.push("  " + group.title);
		for (const s of rows) L.push(`    ${s.name.padEnd(16)} ${short(s.description)}`);
	}

	const other = visible.filter((s) => !assigned.has(s));
	if (other.length) {
		L.push("");
		L.push("  OTHER");
		for (const s of other) L.push(`    ${s.name.padEnd(16)} ${short(s.description)}`);
	}

	if (agents.length) {
		L.push("");
		L.push("AGENTS  (delegate via the `subagent` tool, agent=<name>)");
		for (const a of agents) L.push(`    ${a.name.padEnd(18)} ${short(a.description, 84)}`);
	}

	// Commands: prefer the live list, fall back to the known set; union both.
	const live = liveCommands(pi, ctx);
	const cmds = Array.from(new Set([...live, ...KNOWN_COMMANDS])).sort();
	L.push("");
	L.push("COMMANDS");
	L.push("    " + cmds.join("  "));
	L.push("    Alt+P — cycle model (or /model to pick)");

	if (reference.length) {
		L.push("");
		L.push(`(${reference.length} reference rules auto-load in the background)`);
	}

	L.push("");
	L.push("New? Run /getting-started for the tour.");
	return L.join("\n");
}

function gettingStarted(): string {
	// Mark's voice: direct, concrete, no em-dashes, no slop. Skill names match
	// what's loaded (build/plan/debug/ship/healthcheck/onboard all present).
	return [
		"Welcome to pi-stack. This is your multi-model coding harness — it",
		"remembers what you tell it, delegates to subagents, and ships real PRs.",
		"",
		"Five things to try:",
		"",
		"  1. /skill:onboard",
		"     Seed who you are and what you're working on into memory. Do this",
		"     first — everything else gets sharper once it knows you.",
		"",
		"  2. Build something.",
		"     Describe the task in plain words, then /skill:build. For anything",
		"     bigger than a quick edit, /skill:plan first to shape it.",
		"",
		"  3. /skill:debug",
		"     Hand it a failing test or a bug. It reproduces, root-causes, and",
		"     fixes — no guessing, no symptom patches.",
		"",
		"  4. /skill:ship",
		"     Runs the tests, gets a cross-vendor review from a different model",
		"     than wrote the code, then opens a PR. Stops there — never auto-merges.",
		"",
		"  5. /skill:healthcheck",
		"     Confirms the harness and your code health are both green.",
		"",
		"A few things worth knowing:",
		"  • Memory persists across sessions. /remember to add a fact yourself.",
		"  • /model or Alt+P switches models mid-conversation.",
		"  • /help is the full grouped map of everything loaded.",
	].join("\n");
}

// Fire the first-turn nudge at most once per machine, gated by a marker file.
function maybeNudge(ctx: any): void {
	safe(() => {
		const fs = require("node:fs");
		const path = require("node:path");
		const marker = path.join(agentDir(), NUDGE_MARKER);
		// Atomically claim the marker: fs.openSync(..., "wx") fails with EEXIST if
		// the file already exists, so only the one process that creates it nudges.
		// Two simultaneous first-ever pi processes can't both win the race.
		let claimed = false;
		try {
			fs.mkdirSync(agentDir(), { recursive: true });
			const fd = fs.openSync(marker, "wx");
			fs.writeSync(fd, new Date().toISOString());
			fs.closeSync(fd);
			claimed = true;
		} catch {
			// EEXIST (already nudged) or any fs error → skip. Never throw at load.
			claimed = false;
		}
		if (!claimed) return;
		safe(() =>
			ctx?.ui?.notify?.(
				"New here? Type /help for the map or /getting-started for the tour.",
				"info",
			),
		);
	});
}

export default function (pi: any) {
	const on = (name: string, fn: (e: any, ctx: any) => any) =>
		safe(() => pi.on(name, async (e: any, ctx: any) => safe(() => fn(e, ctx))));

	safe(() =>
		pi.registerCommand("help", {
			description:
				"Live map of the harness: loaded skills (grouped), agents, and commands",
			handler: async (_args: any, ctx: any) =>
				safe(() => ctx?.ui?.notify?.(buildHelp(pi, ctx), "info")),
		}),
	);

	safe(() =>
		pi.registerCommand("getting-started", {
			description: "A warm first-run tour: five things to try and how memory works",
			handler: async (_args: any, ctx: any) =>
				safe(() => ctx?.ui?.notify?.(gettingStarted(), "info")),
		}),
	);

	// First-turn nudge, once per machine. session_start is the earliest safe idle
	// hook; if it never fires we still nudge on the first turn_start.
	let nudged = false;
	const nudgeOnce = (ctx: any) => {
		if (nudged) return;
		nudged = true;
		maybeNudge(ctx);
	};
	on("session_start", (_e, ctx) => nudgeOnce(ctx));
	on("turn_start", (_e, ctx) => nudgeOnce(ctx));
}
