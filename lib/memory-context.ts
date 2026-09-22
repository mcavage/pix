// pix — the two workspace facts memory-recall.ts and memory-capture.ts stamp
// on every memory call, shared so recall and capture can never diverge onto
// a different profile or project than each other.
//
// Both are plain functions rather than module-level constants on purpose:
// each extension instance reads the profile marker EXACTLY ONCE at its own
// load (`const ACTIVE_PROFILE = readActiveProfile()`), so a second sandbox
// overwriting the file mid-session cannot make capture stamp a different
// profile than recall queries, and a test that imports a fresh extension
// instance under a different cwd sees that cwd's marker, not a value cached
// by whichever import ran first.

import { basename, join } from "node:path";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

// The active profile scopes recall/capture (recall sees {profile}∪{default};
// captures stamp it). The launcher writes it to <cwd>/.pix/profile per run;
// absent => "default" (the shared bucket), so an un-launched sandbox keeps the
// backward-compatible behavior. Never throws (try/catch): a missing file is
// the normal, un-scoped case.
export function readActiveProfile(cwd: string = process.cwd()): string {
	try {
		const raw = readFileSync(join(cwd, ".pix", "profile"), "utf8").trim();
		return raw || "default";
	} catch {
		return "default";
	}
}

// The project you're in now, used to scope and boost its memories. Inside the
// sandbox every project mounts at /home/agent/workspace, so the dir name is
// useless; use the git remote (stable across machines), via execFileSync argv
// so shell syntax in a workspace path stays literal. The returned resolver
// caches per extension instance; null = global (no remote, generic dir name).
export function createProjectResolver(): (ctx: any) => string | null {
	let project: string | null | undefined;
	return (ctx: any) => {
		if (project !== undefined) return project;
		const cwd = (typeof ctx?.cwd === "string" && ctx.cwd) || process.cwd();
		try {
			const url = execFileSync("git", ["-C", cwd, "remote", "get-url", "origin"], {
				encoding: "utf8",
				timeout: 1500,
				stdio: ["ignore", "pipe", "ignore"],
			}).trim();
			const name = url.replace(/\.git$/, "").split(/[/:]/).filter(Boolean).pop();
			if (name) return (project = name);
		} catch {}
		const base = basename(cwd);
		return (project = base && base !== "workspace" && base !== "/" ? base : null);
	};
}
