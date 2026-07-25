#!/usr/bin/env node
// Vendored patch — "bounded subagent wait".
//
// @tintinweb/pi-subagents waits on a subagent with a BARE `await record.promise`
// in two places, with no timeout and (in get_subagent_result) an IGNORED abort
// signal (the param is named `_signal`). So when a subagent's gateway/model call
// dies silently — the promise never settles — the await parks pi's event loop
// forever: Esc does nothing and the only recovery is killing the process from the
// host. We hit this three times (Slack enumeration, self-audit, ...).
//
// This rewrites both bare awaits into a Promise.race against (a) the real abort
// signal, so Esc cancels the wait immediately, and (b) a wall-clock timeout
// (PI_SUBAGENT_WAIT_MS, default 180000) as a backstop. On either, the race
// resolves and the caller returns control — get_subagent_result then just reports
// the agent as still "running", which is correct and, crucially, keeps the loop
// alive. The stuck subagent promise is abandoned, not awaited.
//
//   • idempotent — no-ops if the marker is already present;
//   • non-fatal  — if the extension changed, prints a loud warning and exits 0 so
//     the image still builds (you keep the wedge until the script is refreshed).
//
// Run after `pi install "npm:@tintinweb/pi-subagents@..."` (the Dockerfile does).

import { execSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const MARKER = "pix-bounded-wait";
const warn = (m) => console.warn(`[apply-subagent-timeout] ⚠ ${m}`);
const info = (m) => console.log(`[apply-subagent-timeout] ${m}`);

// The race expression. `SIG` is replaced with the in-scope signal variable at
// each site. On abort OR timeout the race resolves (does not reject), so the
// caller continues normally and reports current status.
const race = (sig) =>
	`await Promise.race([record.promise, new Promise((__r) => { const __t = setTimeout(__r, Number(process.env.PI_SUBAGENT_WAIT_MS) || 180000); const __s = ${sig}; if (__s && __s.addEventListener) __s.addEventListener("abort", () => { clearTimeout(__t); __r(); }, { once: true }); })]); /*${MARKER}*/`;

// Each site: a regex anchored on the statement immediately BEFORE the bare await
// (so we hit exactly the right `await record.promise;`), and the signal in scope.
const SITES = [
	{
		file: "index.js", // get_subagent_result handler — signal param is `_signal`
		re: /(cancelNudge\(params\.agent_id\);\s*\n\s*)await record\.promise;/,
		sig: "_signal",
	},
	{
		file: "agent-manager.js", // spawnAndWait — foreground path — `options.signal`
		re: /(const record = this\.agents\.get\(id\);\s*\n\s*)await record\.promise;/,
		sig: "(options && options.signal)",
	},
];

function findDistDir() {
	const rel = "node_modules/@tintinweb/pi-subagents/dist";
	const candidates = [
		join(homedir(), ".pi/agent/npm", rel),
		join(process.env.PI_AGENT_DIR || "", "npm", rel),
	].filter(Boolean);
	for (const c of candidates) if (existsSync(c)) return c;
	try {
		const found = execSync(
			`find ${JSON.stringify(homedir())} -path '*@tintinweb/pi-subagents/dist' -type d 2>/dev/null | head -1`,
			{ encoding: "utf8" },
		).trim();
		if (found && existsSync(found)) return found;
	} catch {
		/* fall through */
	}
	return null;
}

function main() {
	const dist = findDistDir();
	if (!dist) {
		warn("could not locate @tintinweb/pi-subagents/dist — skipping patch.");
		return; // non-fatal
	}
	let patchedAny = false;
	for (const site of SITES) {
		const path = join(dist, site.file);
		if (!existsSync(path)) {
			warn(`missing ${path} — skipping.`);
			continue;
		}
		const src = readFileSync(path, "utf8");
		if (src.includes(MARKER)) {
			info(`already patched: ${site.file}`);
			continue;
		}
		if (!site.re.test(src)) {
			warn(
				`anchor not found in ${site.file} — pi-subagents changed; refresh ` +
					`scripts/patches/apply-subagent-timeout.mjs for this version. Left unpatched.`,
			);
			continue;
		}
		const out = src.replace(site.re, `$1${race(site.sig)}`);
		writeFileSync(path, out);
		if ((out.split(MARKER).length - 1) !== 1) {
			warn(`post-write marker count off in ${site.file}`);
			continue;
		}
		info(`patched ${site.file}`);
		patchedAny = true;
	}
	if (!patchedAny) info("nothing to patch (already applied or anchors gone).");
}

main();
