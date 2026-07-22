#!/usr/bin/env node
// pi-manage-todo-list 0.4.0 clears only in-memory state for `/todos clear`.
// Persist a launcher-owned clear marker so resumed sessions and pi-stack's
// compaction continuation do not resurrect work the user explicitly cleared.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const root = path.join(
	os.homedir(),
	".pi",
	"agent",
	"npm",
	"node_modules",
	"pi-manage-todo-list",
	"dist",
);
const indexPath = path.join(root, "index.js");
const statePath = path.join(root, "state-manager.js");
const marker = "pi-stack-todo-cleared";

function patch(file, before, after) {
	const current = fs.readFileSync(file, "utf8");
	if (current.includes(after)) return false;
	if (!current.includes(before)) {
		throw new Error(`anchor not found in ${file}; refresh apply-todo-durable-clear.mjs`);
	}
	fs.writeFileSync(file, current.replace(before, after));
	return true;
}

const indexChanged = patch(
	indexPath,
	`                state.clear();\n                clearWidget(ctx);`,
	`                state.clear();\n                pi.appendEntry("${marker}", {});\n                clearWidget(ctx);`,
);

const stateChanged = patch(
	statePath,
	`        for (const entry of ctx.sessionManager.getBranch()) {\n            if (entry.type !== "message")\n                continue;`,
	`        for (const entry of ctx.sessionManager.getBranch()) {\n            if (entry.type === "custom" && entry.customType === "${marker}") {\n                this.todos = [];\n                continue;\n            }\n            if (entry.type !== "message")\n                continue;`,
);

console.log(
	indexChanged || stateChanged
		? "[apply-todo-durable-clear] patched"
		: "[apply-todo-durable-clear] already patched",
);
