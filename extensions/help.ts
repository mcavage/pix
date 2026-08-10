// pix — a LIVE map of the harness (`/help`) plus a warm first-run tour
// and a one-time first-turn nudge (points at /help).
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
	{ title: "WORKFLOW", members: ["brainstorm", "plan", "build", "deliver", "ship", "challenge"] },
	{ title: "DEVELOP", members: ["debug", "tdd", "code-review", "peer-review", "verify"] },
	{ title: "QUALITY", members: ["qa", "design-review", "healthcheck"] },
	{
		title: "WRITE",
		members: [
			"anti-slop",
			"writing-voice",
			"one-pager",
			"microcopy",
			"docs-sync",
			"competitive",
		],
	},
	{ title: "DATA", members: ["gworkspace", "ingest", "enrich"] },
	{ title: "SYSTEM", members: ["onboarding", "promote", "model-refresh"] },
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

const NUDGE_MARKER = ".pix-help-nudged";

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

// notify("info") routes to the interactive showStatus(), which wraps the ENTIRE
// message in theme.fg("dim", ...) — and in the default dracula theme `dim` is
// `selection` (#44475a), nearly invisible on the #282a36 background. That is why
// the old plain-text /help read as "faint": with no inline color codes the whole
// block rendered at that dim color. The fix is to give every visible token its
// own readable color so nothing falls through to the dim wrapper:
//   • head  — bold + accent (purple): the title, section, and group headers
//   • accent— accent (purple): command names, inline markers, labels
//   • desc  — muted (comment blue-gray, readable): secondary detail only
//   • text  — forces the terminal DEFAULT foreground (\x1b[39m) so body prose and
//             skill/agent NAMES render bright/normal instead of inheriting dim
// theme.fg() THROWS on an unknown/empty color key, so every call is guarded and
// degrades to the raw string when a theme isn't available.
const RESET_FG = "\x1b[39m";
function painter(ctx: any) {
	const theme = safe(() => ctx?.ui?.theme);
	const wrap = (color: string, s: string): string => {
		const r = safe(() => theme?.fg?.(color, s));
		return typeof r === "string" ? r : s;
	};
	const bold = (s: string): string => {
		const r = safe(() => theme?.bold?.(s));
		return typeof r === "string" ? r : s;
	};
	return {
		head: (s: string) => bold(wrap("accent", s)),
		accent: (s: string) => wrap("accent", s),
		desc: (s: string) => wrap("muted", s),
		warn: (s: string) => wrap("warning", s),
		// Prefix with an explicit reset-to-default so the text is drawn at the
		// terminal's normal foreground (readable) rather than the dim wrapper.
		text: (s: string) => RESET_FG + s,
	};
}

// Wrap a long description into readable lines for the detail view.
function wrapText(s: string, width = 76): string[] {
	const words = s.replace(/\s+/g, " ").trim().split(" ").filter(Boolean);
	const lines: string[] = [];
	let cur = "";
	for (const w of words) {
		if (!cur) cur = w;
		else if ((cur + " " + w).length <= width) cur += " " + w;
		else {
			lines.push(cur);
			cur = w;
		}
	}
	if (cur) lines.push(cur);
	return lines;
}

// Which display bucket a skill lands in (mirrors buildHelp's grouping), for the
// `/help <name>` detail view.
function groupTitleFor(s: SkillInfo): string {
	const keys = [s.dir, s.name];
	if (keys.some((k) => REFERENCE.has(k)))
		return "REFERENCE (auto-loads as a convention)";
	for (const g of GROUPS)
		if (g.members.some((m) => s.dir === m || s.name === m)) return g.title;
	return "OTHER";
}

// Live command objects ({name, description?, sourceInfo?}) if pi exposes them.
function liveCommandInfos(pi: any, ctx: any): any[] {
	const raw =
		safe(() => ctx?.getCommands?.()) ?? safe(() => pi?.getCommands?.()) ?? null;
	if (!Array.isArray(raw)) return [];
	return raw
		.map((c: any) => (typeof c === "string" ? { name: c } : c))
		.filter((c: any) => c && typeof c.name === "string" && c.name.length > 0);
}

// Normalize a query or command name for matching: drop leading `/` and `skill:`.
function normName(x: any): string {
	return String(x).trim().toLowerCase().replace(/^\//, "").replace(/^skill:/, "");
}

// Coerce whatever the command handler hands us for args into a plain string.
function argString(args: any): string {
	if (typeof args === "string") return args.trim();
	if (Array.isArray(args)) return args.join(" ").trim();
	if (args && typeof args === "object") {
		for (const k of ["args", "input", "text", "value", "argument"]) {
			const v = args[k];
			if (typeof v === "string") return v.trim();
			if (Array.isArray(v)) return v.join(" ").trim();
		}
	}
	return "";
}

function buildHelp(pi: any, ctx: any): string {
	const p = painter(ctx);
	const skills = scanSkills();
	const agents = scanAgents();

	const byNameKey = (s: SkillInfo) => new Set([s.dir, s.name]);
	const reference = skills.filter((s) =>
		[...byNameKey(s)].some((k) => REFERENCE.has(k)),
	);
	const visible = skills.filter(
		(s) => ![...byNameKey(s)].some((k) => REFERENCE.has(k)),
	);

	// A row: accent bullet, then the NAME at default (readable) foreground, then a
	// muted one-line description. Padding is done on the raw name so the ANSI codes
	// never throw the alignment off.
	const row = (name: string, description: string, pad: number) =>
		"    " +
		p.accent("·") +
		p.text(" " + name.padEnd(pad)) +
		"  " +
		p.desc(short(description));

	// Assign each visible skill to a group; anything unmatched -> OTHER.
	const assigned = new Set<SkillInfo>();
	const L: string[] = [];
	L.push(p.head("pix — what's loaded right now"));
	L.push(p.desc("Tip: /help <name> for the detail on one skill, agent, or command."));
	L.push("");
	L.push(p.head("SKILLS") + p.desc("  (run /skill:<name>, or just describe the task)"));

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
		L.push("  " + p.head(group.title));
		for (const s of rows) L.push(row(s.name, s.description, 16));
	}

	const other = visible.filter((s) => !assigned.has(s));
	if (other.length) {
		L.push("");
		L.push("  " + p.head("OTHER"));
		for (const s of other) L.push(row(s.name, s.description, 16));
	}

	if (agents.length) {
		L.push("");
		L.push(
			p.head("AGENTS") + p.desc("  (delegate via the `subagent` tool, agent=<name>)"),
		);
		for (const a of agents) L.push(row(a.name, a.description, 18));
	}

	// Commands: prefer the live list, fall back to the known set; union both.
	const live = liveCommands(pi, ctx);
	const cmds = Array.from(new Set([...live, ...KNOWN_COMMANDS])).sort();
	L.push("");
	L.push(p.head("COMMANDS"));
	L.push("    " + cmds.map((c) => p.accent(c)).join(p.desc("  ")));
	L.push("    " + p.accent("Alt+P") + p.desc(" cycle model (or /model to pick)"));

	if (reference.length) {
		L.push("");
		L.push(
			p.desc(
				`(${reference.length} reference rules auto-load in the background — /help <name> for any)`,
			),
		);
	}

	return L.join("\n");
}

// `/help <name>` — full detail for one loaded skill, agent, or command. Reads the
// LOADED set live (frontmatter scan + pi.getCommands()) just like the bare map.
function buildDetail(pi: any, ctx: any, query: string): string {
	const p = painter(ctx);
	const path = require("node:path");
	const q = normName(query);
	if (!q) return buildHelp(pi, ctx);
	const label = (k: string) => p.accent("  " + k.padEnd(9));

	// Skill?
	const skill = scanSkills().find((s) => normName(s.dir) === q || normName(s.name) === q);
	if (skill) {
		const file = path.join(agentDir(), "skills", skill.dir, "SKILL.md");
		const L: string[] = [];
		L.push(p.head(skill.name) + p.desc("   skill"));
		L.push(label("Group") + p.text(groupTitleFor(skill)));
		L.push(
			label("Invoke") +
				p.accent(`/skill:${skill.dir}`) +
				p.desc("   (or just describe the task and let it auto-load)"),
		);
		L.push(label("File") + p.text(file));
		if (skill.description) {
			L.push("");
			for (const ln of wrapText(skill.description)) L.push(p.desc(ln));
		}
		return L.join("\n");
	}

	// Agent?
	const agent = scanAgents().find((a) => normName(a.name) === q);
	if (agent) {
		const file = path.join(agentDir(), "agents", agent.name + ".md");
		const L: string[] = [];
		L.push(p.head(agent.name) + p.desc("   agent"));
		L.push(label("Delegate") + p.text(`subagent tool, agent=${agent.name}`));
		L.push(label("File") + p.text(file));
		if (agent.description) {
			L.push("");
			for (const ln of wrapText(agent.description)) L.push(p.desc(ln));
		}
		return L.join("\n");
	}

	// Command?
	const infos = liveCommandInfos(pi, ctx);
	const known = KNOWN_COMMANDS.map((c) => ({ name: c.replace(/^\//, "") }) as any);
	const cmd = [...infos, ...known].find((c) => normName(c.name) === q);
	if (cmd) {
		const display = "/" + normName(cmd.name);
		const L: string[] = [];
		L.push(p.head(display) + p.desc("   command"));
		if (cmd.description) {
			L.push("");
			for (const ln of wrapText(cmd.description)) L.push(p.desc(ln));
		}
		const cpath = cmd?.sourceInfo?.path;
		if (cpath) L.push(label("File") + p.text(cpath));
		L.push("");
		L.push(p.desc("Type ") + p.accent(display) + p.desc(" at the prompt to run it."));
		return L.join("\n");
	}

	// Not found.
	return [
		p.warn(`No skill, agent, or command matches "${query.trim()}".`),
		p.desc("Run ") + p.accent("/help") + p.desc(" for the full map of what's loaded."),
	].join("\n");
}


// Fire the first-turn nudge at most once per machine, gated by a marker file.
function gettingStartedText(): string {
	return [
		"PIX: GETTING STARTED",
		"",
		"  /help                 live map of loaded skills, agents, and commands",
		"  /skill:<name>         run a workflow directly (try /skill:healthcheck)",
		"  /model                inspect or switch the active model",
		"  /status               session, context, tools, and host-service health",
		"  /recall <query>       inspect relevant persistent memory",
		"",
		"Ask normally for ordinary work. For a rigorous feature, say ‘plan this’,",
		"‘build this’, or ‘ship this’; Pix selects the matching workflow and crew.",
		"Host commands such as pix run and pix task run in your terminal,",
		"not as slash commands inside this agent.",
	].join("\n");
}

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
				"New here? Type /help for the map.",
				"info",
			),
		);
	});
}

export default function (pi: any) {
	const on = (name: string, fn: (e: any, ctx: any) => any) =>
		safe(() => pi.on(name, async (e: any, ctx: any) => safe(() => fn(e, ctx))));

	safe(() =>
		pi.registerCommand("getting-started", {
			description: "Short first-session tour: core slash commands and how to invoke workflows",
			handler: async (_args: any, ctx: any) =>
				safe(() => ctx?.ui?.notify?.(gettingStartedText(), "info")),
		}),
	);

	safe(() =>
		pi.registerCommand("help", {
			description:
				"Live map of the harness (skills/agents/commands); /help <name> for detail on one",
			handler: async (args: any, ctx: any) => {
				const query = argString(args);
				const text = query ? buildDetail(pi, ctx, query) : buildHelp(pi, ctx);
				return safe(() => ctx?.ui?.notify?.(text, "info"));
			},
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
