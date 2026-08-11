// pix — durable, named output styles for user-visible prose.
//
// Personal style files live in Pix's existing RW personal-context mount, not in
// the disposable agent home and not in config.toml. The active style travels to
// the model as an append-only hidden message so it does not rewrite the system
// prompt or churn the provider's prefix cache on every turn.

import { randomBytes } from "node:crypto";
import {
	existsSync,
	lstatSync,
	mkdirSync,
	readdirSync,
	readFileSync,
	renameSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import path from "node:path";
import { StringEnum } from "@earendil-works/pi-ai";
import { withFileMutationQueue } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import {
	OUTPUT_STYLE_CUSTOM_TYPE,
	STYLE_BODY_MAX_BYTES,
	findPersonalContextDir,
	parseStyleFile,
	renderStyleBlock,
	renderStyleRevocation,
	serializeStyleFile,
	slugifyStyleName,
	styleFingerprint,
	validateStyleSlug,
	type OutputStyle,
} from "../lib/output-style.ts";

const ACTIVE_FILE = "active";
const STYLES_DIR = "output-styles";
const ACTIVE_FILE_MAX_BYTES = 256;
const STYLE_FILE_MAX_BYTES = 16 * 1024;

interface ExtensionOptions {
	contextDir?: string | null;
}

interface ActiveStyle {
	slug: string;
	style: OutputStyle;
	source: string;
	fingerprint: string;
}

function toolResult(text: string, details: Record<string, unknown> = {}) {
	return { content: [{ type: "text", text }], details };
}

function isError(value: ReturnType<typeof parseStyleFile>): value is { error: string } {
	return "error" in value;
}

export function registerOutputStyleExtension(pi: any, options: ExtensionOptions = {}) {
	const contextDir = options.contextDir === undefined
		? findPersonalContextDir(process.argv, process.env)
		: options.contextDir;
	const stylesDir = contextDir ? path.join(contextDir, STYLES_DIR) : null;
	let lastDeliveredKey: string | null = null;
	let lastNotifiedKey: string | null = null;
	let lastErrorNotice: string | null = null;
	let hadActiveStyle = false;
	let pendingRevocation = false;

	function lstatIfPresent(file: string): ReturnType<typeof lstatSync> | null {
		try {
			return lstatSync(file);
		} catch (error) {
			if ((error as NodeJS.ErrnoException)?.code === "ENOENT") return null;
			throw error;
		}
	}

	function requireStylesDir(): string {
		if (!stylesDir) {
			throw new Error("Pix personal context is not mounted; output styles cannot persist in this session.");
		}
		const stat = lstatIfPresent(stylesDir);
		if (stat) {
			if (stat.isSymbolicLink() || !stat.isDirectory()) {
				throw new Error(`Output-style path is not a real directory: ${stylesDir}`);
			}
		} else {
			mkdirSync(stylesDir, { recursive: true });
		}
		return stylesDir;
	}

	function activePath(): string {
		return path.join(requireStylesDir(), ACTIVE_FILE);
	}

	function stylePath(slug: string): string {
		return path.join(requireStylesDir(), `${slug}.md`);
	}

	function assertRegularFile(file: string, maxBytes: number): void {
		const stat = lstatSync(file);
		if (stat.isSymbolicLink() || !stat.isFile()) throw new Error(`Output style is not a regular file: ${file}`);
		if (stat.size > maxBytes) throw new Error(`Output-style file is too large: ${path.basename(file)}`);
	}

	function atomicWrite(file: string, content: string): void {
		if (lstatIfPresent(file)?.isSymbolicLink()) {
			throw new Error(`Refusing to replace symlinked output-style file: ${file}`);
		}
		const temp = `${file}.tmp-${process.pid}-${randomBytes(8).toString("hex")}`;
		try {
			writeFileSync(temp, content, { encoding: "utf8", mode: 0o600, flag: "wx" });
			renameSync(temp, file);
		} finally {
			rmSync(temp, { force: true });
		}
	}

	function requireSlug(input: string): string {
		const slug = validateStyleSlug(input);
		if (!slug) throw new Error("Output-style slug must use 1-64 lowercase letters, numbers, or hyphens.");
		return slug;
	}

	function readStyleBySlug(slugInput: string): ActiveStyle {
		const slug = requireSlug(slugInput);
		const file = stylePath(slug);
		if (!existsSync(file)) throw new Error(`Unknown output style: ${slug}`);
		assertRegularFile(file, STYLE_FILE_MAX_BYTES);
		const source = readFileSync(file, "utf8");
		const parsed = parseStyleFile(source);
		if (isError(parsed)) throw new Error(`${slug}: ${parsed.error}`);
		return { slug, style: parsed, source, fingerprint: styleFingerprint(slug, source) };
	}

	function resolveStyleSlug(input: string): string {
		const candidates = [validateStyleSlug(input), slugifyStyleName(input)].filter(
			(value, index, all): value is string => Boolean(value) && all.indexOf(value) === index,
		);
		for (const slug of candidates) {
			if (existsSync(stylePath(slug))) return slug;
		}
		const wantedName = input.trim().toLowerCase();
		const dir = requireStylesDir();
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			if (!entry.isFile() || !entry.name.endsWith(".md")) continue;
			const slug = entry.name.slice(0, -3);
			if (validateStyleSlug(slug) !== slug) continue;
			try {
				if (readStyleBySlug(slug).style.name.toLowerCase() === wantedName) return slug;
			} catch {
				// Malformed files are reported by list; they cannot be activated.
			}
		}
		throw new Error(`Unknown output style: ${input}`);
	}

	function readStyle(nameOrSlug: string): ActiveStyle {
		return readStyleBySlug(resolveStyleSlug(nameOrSlug));
	}

	function readActiveStyle(): ActiveStyle | null {
		if (!stylesDir || !existsSync(stylesDir)) return null;
		const pointer = path.join(stylesDir, ACTIVE_FILE);
		if (!existsSync(pointer)) return null;
		assertRegularFile(pointer, ACTIVE_FILE_MAX_BYTES);
		const raw = readFileSync(pointer, "utf8").trim();
		if (!raw) return null;
		return readStyleBySlug(requireSlug(raw));
	}

	function listStyles(): {
		styles: Array<{ slug: string; active: boolean; style: OutputStyle }>;
		warnings: string[];
	} {
		const dir = requireStylesDir();
		const warnings: string[] = [];
		let activeSlug: string | null = null;
		try {
			activeSlug = readActiveStyle()?.slug ?? null;
		} catch (error) {
			warnings.push(`active: ${error instanceof Error ? error.message : String(error)}`);
		}
		const styles: Array<{ slug: string; active: boolean; style: OutputStyle }> = [];
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			if (!entry.isFile() || !entry.name.endsWith(".md")) continue;
			const slug = entry.name.slice(0, -3);
			if (validateStyleSlug(slug) !== slug) {
				warnings.push(`${entry.name}: invalid style filename`);
				continue;
			}
			try {
				styles.push({ slug, active: slug === activeSlug, style: readStyleBySlug(slug).style });
			} catch (error) {
				warnings.push(error instanceof Error ? error.message : String(error));
			}
		}
		return { styles: styles.sort((a, b) => a.slug.localeCompare(b.slug)), warnings };
	}

	async function setActive(slugInput: string): Promise<ActiveStyle> {
		const active = readStyle(slugInput);
		const pointer = activePath();
		await withFileMutationQueue(pointer, async () => atomicWrite(pointer, `${active.slug}\n`));
		hadActiveStyle = true;
		pendingRevocation = false;
		return active;
	}

	async function saveAndActivate(name: string, description: string, instructions: string): Promise<ActiveStyle> {
		const slug = slugifyStyleName(name);
		if (!slug) throw new Error("Output-style name must produce a 1-64 character lowercase slug.");
		const body = typeof instructions === "string" ? instructions.trim() : "";
		if (!body) throw new Error("Output-style instructions are required.");
		if (Buffer.byteLength(body, "utf8") > STYLE_BODY_MAX_BYTES) {
			throw new Error(`Output-style instructions are too large (max ${STYLE_BODY_MAX_BYTES} UTF-8 bytes).`);
		}
		const style: OutputStyle = {
			name: name.replace(/[\r\n]+/g, " ").trim().slice(0, 120),
			description: (description ?? "").replace(/[\r\n]+/g, " ").trim().slice(0, 240),
			body,
		};
		const file = stylePath(slug);
		const source = serializeStyleFile(style);
		await withFileMutationQueue(file, async () => atomicWrite(file, source));
		return setActive(slug);
	}

	async function disable(): Promise<boolean> {
		if (!stylesDir) requireStylesDir();
		const pointer = path.join(stylesDir!, ACTIVE_FILE);
		const wasActive = existsSync(pointer) || hadActiveStyle;
		await withFileMutationQueue(pointer, async () => rmSync(pointer, { force: true }));
		pendingRevocation = wasActive;
		hadActiveStyle = false;
		return wasActive;
	}

	function formattedList(): string {
		const { styles, warnings } = listStyles();
		const lines = styles.length
			? styles.map(({ slug, active, style }) => `${slug}${active ? " (active)" : ""}${style.description ? ` — ${style.description}` : ""}`)
			: ["No valid output styles saved."];
		if (warnings.length) lines.push("Warnings:", ...warnings.map((warning) => `- ${warning}`));
		return lines.join("\n");
	}

	function notifyActive(ctx: any, active: ActiveStyle, message: string): void {
		ctx?.ui?.notify?.(message, "info");
		lastNotifiedKey = `active:${active.fingerprint}`;
		lastErrorNotice = null;
	}

	pi.registerTool({
		name: "output_style",
		label: "Output Style",
		description:
			"Create, list, activate, or disable a durable Pix output style. Use save only when the user directly asks to set or create an output style; save also activates it. Styles affect user-visible prose, not facts, tools, permissions, code, quoted text, or task scope.",
		promptSnippet: "Manage the user's durable Pix output style when they directly ask to set, switch, list, or disable one",
		promptGuidelines: [
			"Use output_style only for a direct user request about Pix output style. Never claim a style was saved or activated unless the output_style tool succeeds.",
		],
		parameters: Type.Object({
			action: StringEnum(["list", "save", "activate", "off"] as const),
			name: Type.Optional(Type.String({ description: "Style display name for save, or saved style slug for activate." })),
			description: Type.Optional(Type.String({ description: "One-line description used when saving a style." })),
			instructions: Type.Optional(Type.String({ description: "Markdown style instructions. Required for save." })),
		}),
		async execute(_toolCallId: string, params: any, _signal: AbortSignal | undefined, _onUpdate: unknown, ctx: any) {
			switch (params.action) {
				case "list":
					return toolResult(formattedList(), { action: "list" });
				case "save": {
					if (!params.name) throw new Error("Output-style name is required for save.");
					const active = await saveAndActivate(params.name, params.description ?? "", params.instructions ?? "");
					notifyActive(ctx, active, `Output style "${active.slug}" saved and activated.`);
					return toolResult(
						`Output style "${active.slug}" saved and activated. Apply these instructions beginning with the next user-visible prose in this run.\n\n${renderStyleBlock(active.style)}`,
						{ action: "save", slug: active.slug, active: true },
					);
				}
				case "activate": {
					if (!params.name) throw new Error("Output-style name is required for activate.");
					const active = await setActive(params.name);
					notifyActive(ctx, active, `Output style "${active.slug}" activated.`);
					return toolResult(
						`Output style "${active.slug}" activated. Apply these instructions beginning with the next user-visible prose in this run.\n\n${renderStyleBlock(active.style)}`,
						{ action: "activate", slug: active.slug, active: true },
					);
				}
				case "off": {
					const changed = await disable();
					ctx?.ui?.notify?.(changed ? "Output style disabled." : "Output style was already disabled.", "info");
					lastNotifiedKey = "off";
					return toolResult(changed ? "Output style disabled." : "Output style was already disabled.", { action: "off", changed });
				}
				default:
					throw new Error(`Unknown output-style action: ${String(params.action)}`);
			}
		},
	});

	pi.registerCommand("output-style", {
		description: "List, activate, or disable durable Pix output styles",
		async handler(args: string, ctx: any) {
			try {
				const requested = typeof args === "string" ? args.trim() : "";
				if (!requested || requested === "list") {
					ctx.ui.notify(formattedList(), "info");
					return;
				}
				if (requested === "off") {
					const changed = await disable();
					ctx.ui.notify(changed ? "Output style disabled." : "Output style was already disabled.", "info");
					return;
				}
				const active = await setActive(requested);
				notifyActive(ctx, active, `Output style "${active.slug}" activated.`);
			} catch (error) {
				ctx.ui.notify(error instanceof Error ? error.message : String(error), "error");
			}
		},
	});

	pi.on("before_agent_start", async (_event: any, ctx: any) => {
		try {
			const active = readActiveStyle();
			if (active) {
				hadActiveStyle = true;
				pendingRevocation = false;
				const key = `active:${active.fingerprint}`;
				if (lastNotifiedKey !== key) notifyActive(ctx, active, `Output style active: ${active.slug}.`);
				if (lastDeliveredKey === key) return undefined;
				lastDeliveredKey = key;
				return {
					message: {
						customType: OUTPUT_STYLE_CUSTOM_TYPE,
						display: false,
						content: renderStyleBlock(active.style),
					},
				};
			}
			if (!pendingRevocation && !hadActiveStyle) return undefined;
			const key = "off";
			if (lastDeliveredKey === key) return undefined;
			lastDeliveredKey = key;
			pendingRevocation = false;
			hadActiveStyle = false;
			return {
				message: {
					customType: OUTPUT_STYLE_CUSTOM_TYPE,
					display: false,
					content: renderStyleRevocation(),
				},
			};
		} catch (error) {
			const message = error instanceof Error ? error.message : String(error);
			if (lastErrorNotice !== message) {
				ctx?.ui?.notify?.(`Output style unavailable: ${message}`, "warning");
				lastErrorNotice = message;
			}
			return undefined; // Personal style errors must never block the agent.
		}
	});

	pi.on("session_compact", () => {
		// Compaction can summarize the hidden style message away. Re-append the
		// current state once on the next user turn; never filter old messages.
		lastDeliveredKey = null;
	});
}

export default function outputStyleExtension(pi: any) {
	registerOutputStyleExtension(pi);
}
