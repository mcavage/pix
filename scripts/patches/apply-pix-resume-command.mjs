#!/usr/bin/env node
// pi's default shutdown hint names the in-sandbox `pi` binary. When pix sets
// PIX_RESUME_COMMAND, print one exact host command instead, with the session id
// and workspace that own it. This avoids `-c`, which means newest session and
// can select a different conversation.

import fs from "node:fs";

const target = process.env.PI_INTERACTIVE_MODE ||
	"/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/dist/modes/interactive/interactive-mode.js";
const marker = "pix host resume command";
const before = `    const args = [APP_NAME];
    if (!sessionManager.usesDefaultSessionDir()) {
        args.push("--session-dir", quoteIfNeeded(sessionManager.getSessionDir()));
    }
    args.push("--session", sessionManager.getSessionId());
    return args.join(" ");`;
const after = `    // ${marker}: the terminal belongs to the host launcher.
    const hostResumeCommand = process.env.PIX_RESUME_COMMAND;
    if (hostResumeCommand && !sessionManager.usesDefaultSessionDir()) {
        return \`\${hostResumeCommand} \${quoteIfNeeded(sessionManager.getSessionId())} \${quoteIfNeeded(process.cwd())}\`;
    }
    const args = [APP_NAME];
    if (!sessionManager.usesDefaultSessionDir()) {
        args.push("--session-dir", quoteIfNeeded(sessionManager.getSessionDir()));
    }
    args.push("--session", sessionManager.getSessionId());
    return args.join(" ");`;

const source = fs.readFileSync(target, "utf8");
if (source.includes(marker)) {
	console.log("[apply-pix-resume-command] already patched");
} else {
	if (!source.includes(before)) {
		throw new Error(`anchor not found in ${target}; refresh apply-pix-resume-command.mjs`);
	}
	fs.writeFileSync(target, source.replace(before, after));
	console.log("[apply-pix-resume-command] patched");
}
