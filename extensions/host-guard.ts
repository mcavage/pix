// pix — host-mode guard extension.
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ THIS IS GUARDRAILS AGAINST ACCIDENTS. IT IS NOT A SECURITY BOUNDARY.     │
// │                                                                         │
// │ pi has no built-in permission prompts and no built-in sandbox — the     │
// │ tool_call hook is the only in-process seam, and it is a command-TEXT    │
// │ denylist. It cannot reason about shell semantics: `python -c`, `node    │
// │ -e`, aliases, redirection tricks, symlinks, build scripts, and Docker   │
// │ mounts all slip past pattern matching. It reduces ACCIDENTAL damage and │
// │ makes the loss of the sandbox visible; it does not contain a            │
// │ prompt-injected or adversarial agent. See docs/design/host-mode.md      │
// │ ("Safety posture") before trusting this file for anything more.         │
// │                                                                         │
// │ SECURITY-CRITICAL for a different reason: the Go host launcher          │
// │ (hostrun.go) REFUSES to start `pix host` if this exact file is     │
// │ missing. Don't rename/move it without updating that check.             │
// └─────────────────────────────────────────────────────────────────────────┘
//
// Policy (best-effort, see docs/design/host-mode.md "Guard extension"):
//   - AUTO-ALLOW reads (read/grep/find/ls) unconditionally — never prompt for
//     read-only calls, or every turn turns into a click-through.
//   - AUTO-ALLOW write/edit whose resolved path is INSIDE the workspace root
//     (resolved once at load, from the launch cwd).
//   - BLOCK (no prompt) write/edit whose resolved path ESCAPES the workspace
//     root — host mode confines writes to the launch workspace.
//   - CONFIRM-OR-BLOCK bash commands matching the irreversible set: rm -rf
//     above the workspace, sudo, curl|sh / wget|sh pipes, writes to shell rc
//     files or /etc/sandbox-persistent.sh, git push --force(-with-lease),
//     branch delete, history rewrite (reset --hard, filter-branch/-repo),
//     disk/partition tools, global package installs. If ctx.ui.confirm is
//     unavailable (headless) or throws, FAIL CLOSED (block) — never silently
//     allow the irreversible set.
//   - Every branch is wrapped in try/catch. An internal error while evaluating
//     bash/write/edit fails CLOSED (block + explain); reads and unrecognized
//     tools degrade to "not our concern" rather than throwing, so this
//     extension can never break pi startup or a turn.

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// ─── Workspace root: resolved ONCE at load, from the launch cwd ─────────────
// process.cwd() itself can throw (ENOENT when the launch dir was deleted out
// from under us). That must not crash the load — and it must fail CLOSED: the
// sentinel contains a NUL byte, which no real filesystem path can, so
// isInsideWorkspace() is false for every resolvable path and all writes are
// blocked until relaunched from a real directory.
const WORKSPACE_ROOT_RAW: string = (() => {
	try {
		return process.cwd();
	} catch {
		return "";
	}
})();
// Exported for tests: given "" (cwd unavailable) it returns the blocking
// sentinel; otherwise the realpath'd (or lexically resolved) root.
export function computeWorkspaceRoot(raw: string): string {
	if (!raw) return `${path.sep}\0host-guard-no-workspace`;
	try {
		return fs.realpathSync(raw);
	} catch {
		return path.resolve(raw);
	}
}
const WORKSPACE_ROOT: string = computeWorkspaceRoot(WORKSPACE_ROOT_RAW);

// Exported (parameterized on the root) so the containment logic itself is unit
// tested; the module-level wrappers below bind WORKSPACE_ROOT.
export function isInsideRoot(resolved: string, root: string): boolean {
	return resolved === root || resolved.startsWith(root + path.sep);
}

function isInsideWorkspace(resolved: string): boolean {
	return isInsideRoot(resolved, WORKSPACE_ROOT);
}

// Resolve a (possibly relative, possibly nonexistent) path against a base dir.
// realpath the result when the target exists (so a symlink out of the
// workspace can't defeat the check). For a not-yet-created target (the common
// `write` case) realpathSync throws ENOENT — but a LEXICAL fallback would miss
// a symlinked PARENT (workspace/outside -> /tmp/outside; writing
// outside/new-file escapes). So walk UP to the nearest existing ancestor,
// realpath THAT, and rejoin the nonexistent suffix — containment is then
// checked on the real parent chain.
export function resolvePath(p: string, base: string): string {
	let expanded = p;
	if (expanded === "~" || expanded.startsWith("~/")) {
		expanded = path.join(os.homedir(), expanded.slice(1));
	}
	const abs = path.isAbsolute(expanded) ? expanded : path.resolve(base, expanded);
	try {
		return fs.realpathSync(abs);
	} catch {
		/* target doesn't exist yet — resolve through the nearest real ancestor */
	}
	let cur = path.normalize(abs);
	let suffix = "";
	while (true) {
		const parent = path.dirname(cur);
		suffix = suffix ? path.join(path.basename(cur), suffix) : path.basename(cur);
		try {
			return path.join(fs.realpathSync(parent), suffix);
		} catch {
			/* this ancestor doesn't exist either; keep walking up */
		}
		if (parent === cur) return path.normalize(abs); // hit the root; give up lexically
		cur = parent;
	}
}

// ─── Bash irreversible-set matching (best-effort text patterns) ────────────
interface Hit {
	label: string;
}

function isRmRecursiveForce(cmd: string): boolean {
	if (!/\brm\b/i.test(cmd)) return false;
	// Common flag spellings: -rf, -fr, -Rf, --recursive ... --force (either
	// order), or -r and -f as separate short flags anywhere in the invocation.
	return (
		/\brm\b[^\n;&|]*-[a-zA-Z]*[rR][a-zA-Z]*f[a-zA-Z]*\b/.test(cmd) ||
		/\brm\b[^\n;&|]*-[a-zA-Z]*f[a-zA-Z]*[rR][a-zA-Z]*\b/.test(cmd) ||
		(/\brm\b[^\n;&|]*(--recursive\b|-[a-zA-Z]*[rR]\b)/.test(cmd) &&
			/\brm\b[^\n;&|]*(--force\b|-[a-zA-Z]*f\b)/.test(cmd))
	);
}

// Shell metacharacters we do NOT interpret. A token carrying any of these
// (expansion, substitution, escapes, globs, stray quotes) is UNPARSEABLE by a
// text matcher — `"$HOME"`, `$(cmd)`, backticks, `*` could resolve anywhere —
// so the parse FAILS CLOSED: rmRfHitFor treats the command as out-of-scope
// (confirm-or-block), never as in-workspace.
const SHELL_META = /[$`\\*?\[\]{}]/;

export interface RmParse {
	targets: string[];
	unsafe: boolean; // a target we cannot parse as a plain literal path
}

// Pull the plausible target paths out of an `rm ...` invocation (stops at the
// next shell separator). Plain literal tokens (optionally wrapped in ONE pair
// of matching quotes, which are stripped) become targets; anything with shell
// metacharacters or unbalanced quoting marks the parse unsafe.
export function extractRmTargets(cmd: string): RmParse {
	const targets: string[] = [];
	let unsafe = false;
	const rmSegments = cmd.match(/\brm\b[^\n;&|]*/gi) ?? [];
	for (const segment of rmSegments) {
		const tokens = segment.split(/\s+/).filter(Boolean).slice(1); // drop "rm"
		for (let t of tokens) {
			// Strip ONE pair of matching outer quotes (`rm -rf "/"` → `/`) so the
			// literal inside is still containment-checked.
			if (
				t.length >= 2 &&
				(t[0] === '"' || t[0] === "'") &&
				t[t.length - 1] === t[0]
			) {
				t = t.slice(1, -1);
				if (t.length === 0) continue;
			}
			// Anything still carrying a quote (unbalanced / embedded), expansion,
			// substitution, escape, or glob metachar is not a plain literal path.
			if (t.includes('"') || t.includes("'") || SHELL_META.test(t)) {
				unsafe = true;
				continue;
			}
			if (t.startsWith("-")) continue;
			targets.push(t);
		}
	}
	return { targets, unsafe };
}

export function rmRfHitFor(cmd: string, root: string): Hit | null {
	if (!isRmRecursiveForce(cmd)) return null;
	const { targets, unsafe } = extractRmTargets(cmd);
	// A target we couldn't parse as a literal (quotes, $VAR, $(cmd), backticks,
	// escapes, globs) could expand to ANYWHERE — fail closed.
	if (unsafe) {
		return {
			label:
				"rm -rf with a quoted/expanded/glob target this guard cannot parse (unknown scope)",
		};
	}
	// No parseable target at all is unknown, not safe.
	if (targets.length === 0) {
		return { label: "rm -rf with no directly parseable target (unknown scope)" };
	}
	const outside = targets.some((t) => {
		const resolved = resolvePath(t, root);
		return !isInsideRoot(resolved, root);
	});
	if (outside) {
		return { label: "rm -rf targeting a path outside the workspace" };
	}
	return null;
}

const RC_FILE_PATTERN =
	/(\.bashrc|\.bash_profile|\.bash_login|\.zshrc|\.zprofile|\.zlogin|\.profile)\b|\/etc\/(profile|bash\.bashrc|zsh\/zshrc)\b|\/etc\/sandbox-persistent\.sh\b/i;

const SIMPLE_PATTERNS: Array<{ label: string; test: (cmd: string) => boolean }> = [
	{
		label: "sudo",
		test: (c) => /\bsudo\b/i.test(c),
	},
	{
		label: "curl|sh or wget|sh pipe-to-shell",
		test: (c) =>
			/\b(curl|wget)\b[^\n]*\|\s*(sudo\s+)?(sh|bash|zsh|dash)\b/i.test(c),
	},
	{
		// ANY command naming an rc file / sandbox-persistent.sh is confirm-or-
		// block. Requiring a redirection (the old rule) let `cp payload ~/.bashrc`,
		// `mv ... ~/.bashrc`, and `sed -i ... ~/.bashrc` through untouched. A read
		// (`cat ~/.bashrc`) now prompts too — acceptable cost for failing closed on
		// a text matcher that cannot tell cp from cat reliably.
		label: "command touching a shell rc file or /etc/sandbox-persistent.sh",
		test: (c) => RC_FILE_PATTERN.test(c),
	},
	{
		label: "git push --force / --force-with-lease",
		test: (c) => /\bgit\s+push\b[^\n]*(--force(-with-lease)?\b|\s-f\b)/i.test(c),
	},
	{
		label: "git branch force-delete (-D) or push :ref / --delete",
		test: (c) =>
			/\bgit\s+branch\b[^\n]*-D\b/.test(c) ||
			(/\bgit\s+push\b/i.test(c) && /(--delete\b|\s:[^\s]+)/.test(c)),
	},
	{
		label: "git history rewrite (reset --hard / filter-branch / filter-repo)",
		test: (c) =>
			/\bgit\s+reset\b[^\n]*--hard\b/i.test(c) ||
			/\bgit\s+filter-branch\b/i.test(c) ||
			/\bgit\s+filter-repo\b/i.test(c),
	},
	{
		label: "disk/partition tool (dd/mkfs/fdisk/parted/gdisk/diskutil)",
		test: (c) =>
			/\bdd\s+if=/i.test(c) ||
			/\bmkfs(\.\w+)?\b/i.test(c) ||
			/\b(fdisk|parted|gdisk)\b/i.test(c) ||
			/\bdiskutil\b[^\n]*(erase|partition|reformat)\b/i.test(c),
	},
	{
		label: "global package install (npm/yarn/pnpm -g, brew install, pip as root)",
		test: (c) =>
			/\b(npm|yarn|pnpm)\s+(i|install|add)\b[^\n]*(-g\b|--global\b)/i.test(c) ||
			/\bbrew\s+install\b/i.test(c) ||
			/\bsudo\s+pip3?\s+install\b/i.test(c),
	},
];

export function matchIrreversibleFor(cmd: string, root: string): Hit | null {
	const rm = rmRfHitFor(cmd, root);
	if (rm) return rm;
	for (const p of SIMPLE_PATTERNS) {
		if (p.test(cmd)) return { label: p.label };
	}
	return null;
}

function matchIrreversible(cmd: string): Hit | null {
	return matchIrreversibleFor(cmd, WORKSPACE_ROOT);
}

// ─── tool_call handlers ─────────────────────────────────────────────────────
async function handleBash(cmd: string, ctx: any): Promise<{ block: boolean; reason?: string } | undefined> {
	const hit = matchIrreversible(cmd);
	if (!hit) return undefined; // not in the irreversible set: pass through

	const truncated = cmd.length > 300 ? `${cmd.slice(0, 300)}…` : cmd;
	const hasConfirm = Boolean(ctx?.hasUI) && typeof ctx?.ui?.confirm === "function";
	if (!hasConfirm) {
		// No UI to confirm with: fail closed for the irreversible set. Never
		// silently allow just because we can't ask.
		return {
			block: true,
			reason: `host-guard: blocked (no UI to confirm; failing closed) — ${hit.label}. Guardrails, not a boundary — see docs/design/host-mode.md.`,
		};
	}
	let allowed = false;
	try {
		allowed = await ctx.ui.confirm(
			"⚠ HOST MODE — irreversible command",
			`${hit.label}\n\n${truncated}\n\nThis is a guardrail against accidents, not a security boundary. Allow?`,
		);
	} catch {
		allowed = false; // confirm() throwing must fail closed, not open
	}
	if (!allowed) {
		return { block: true, reason: `host-guard: blocked (declined) — ${hit.label}` };
	}
	return undefined;
}

function handleWriteEdit(toolName: string, rawPath: unknown, ctx: any): { block: boolean; reason?: string } | undefined {
	if (typeof rawPath !== "string" || !rawPath) return undefined; // nothing to check
	const base =
		(ctx && typeof ctx.cwd === "string" && ctx.cwd.trim()) || WORKSPACE_ROOT;
	const { block, resolved } = checkWriteEditPath(rawPath, base, WORKSPACE_ROOT);
	if (!block) return undefined; // auto-allow, no prompt
	return {
		block: true,
		reason: `host-guard: blocked ${toolName} outside the workspace (${WORKSPACE_ROOT}): ${resolved}. Host mode confines writes to the launch workspace — see docs/design/host-mode.md.`,
	};
}

// checkWriteEditPath is the pure write/edit path jail (exported for tests):
// allow inside root, block outside — both on the symlink-resolved path.
export function checkWriteEditPath(
	rawPath: string,
	base: string,
	root: string,
): { block: boolean; resolved: string } {
	const resolved = resolvePath(rawPath, base);
	return { block: !isInsideRoot(resolved, root), resolved };
}

export default function (pi: ExtensionAPI) {
	// HOST-MODE ONLY. This file lives in extensions/, which the Dockerfile bakes
	// and pi auto-discovers in EVERY sandbox — but this guard must exist ONLY under
	// `pix host` (unsandboxed). Inside the disposable VM full-auto no-prompt
	// is the whole point (the sandbox IS the boundary), so a "HOST MODE" confirm
	// on rm/sudo there is both wrong and alarming. The Go host launcher sets
	// OLLAMA_HOSTMODE=1 (the same sentinel status.ts keys the HOST badge on);
	// absent it we register NOTHING and the sandbox behaves exactly as before.
	const hostMode =
		typeof process !== "undefined" && process.env?.OLLAMA_HOSTMODE === "1";
	if (!hostMode) return;
	try {
		pi.on("tool_call", async (event: any, ctx: any) => {
			const toolName = event?.toolName;
			try {
				// Reads: auto-allow, never prompt.
				if (
					toolName === "read" ||
					toolName === "grep" ||
					toolName === "find" ||
					toolName === "ls"
				) {
					return undefined;
				}
				if (toolName === "write" || toolName === "edit") {
					const rawPath = event?.input?.path ?? event?.input?.file_path;
					return handleWriteEdit(toolName, rawPath, ctx);
				}
				if (toolName === "bash") {
					const command = String(event?.input?.command ?? "");
					if (!command) return undefined;
					return await handleBash(command, ctx);
				}
				return undefined; // not our concern (subagent, custom tools, ...)
			} catch (err) {
				// FAIL CLOSED for the irreversible-prone set on any internal error;
				// everything else degrades to "allow" rather than breaking the turn.
				if (toolName === "bash" || toolName === "write" || toolName === "edit") {
					return {
						block: true,
						reason: `host-guard: internal error evaluating this call, failing closed — ${String(err)}`,
					};
				}
				return undefined;
			}
		});
	} catch (err) {
		// If pi.on itself throws (unexpected API shape), the guard did NOT
		// register — and this file existing on disk is exactly what convinced the
		// Go launcher (hostrun.go) it was safe to launch. Swallowing this would
		// leave the session silently UNGUARDED. Fail closed instead: log loudly
		// and rethrow, so the extension load (and with it pi startup) fails and
		// host mode never runs without a working guard.
		try {
			process.stderr.write(
				`\n[host-guard] FATAL: could not register the tool_call guard (${String(err)}).\n` +
					"[host-guard] Refusing to run UNGUARDED — host mode requires a working guard.\n",
			);
		} catch {
			/* stderr itself failing must not mask the rethrow below */
		}
		throw err;
	}
}
