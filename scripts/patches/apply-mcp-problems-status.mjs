#!/usr/bin/env node
// pi-mcp-adapter's footer is either always visible or always hidden. Add a
// problems-only mode: healthy connections stay quiet, while a failed or dropped
// connection remains visible. Also suppress the healthy startup toast in this
// mode; per-server failures keep their existing error notifications.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const root = process.env.PI_MCP_ADAPTER_DIR || path.join(
	os.homedir(), ".pi", "agent", "npm", "node_modules", "pi-mcp-adapter",
);
const initPath = path.join(root, "init.ts");
const typesPath = path.join(root, "types.ts");
const marker = "pix MCP problems-only status";

function patch(file, before, after) {
	const source = fs.readFileSync(file, "utf8");
	if (source.includes(marker)) return false;
	if (!source.includes(before)) {
		throw new Error(`anchor not found in ${file}; refresh apply-mcp-problems-status.mjs`);
	}
	fs.writeFileSync(file, source.replace(before, after));
	return true;
}

const typesChanged = patch(
	typesPath,
	`export type McpFooterStatus = "full" | "compact" | "off";`,
	`// ${marker}\nexport type McpFooterStatus = "full" | "compact" | "problems" | "off";`,
);

const startupBefore = `  if (ui && connectedCount > 0) {`;
const startupAfter = `  // ${marker}: a healthy startup needs no toast.\n  if (ui && connectedCount > 0 && (failedCount > 0 || state.config.settings?.mcpFooterStatus !== "problems")) {`;
const initWithStartup = fs.readFileSync(initPath, "utf8");
let initChanged = false;
let next = initWithStartup;
if (!next.includes(marker)) {
	if (!next.includes(startupBefore)) {
		throw new Error(`anchor not found in ${initPath}; refresh apply-mcp-problems-status.mjs`);
	}
	next = next.replace(startupBefore, startupAfter);
	initChanged = true;
}

const statusBefore = `  if (footerStatus === "off") {
    ui.setStatus("mcp", undefined);
    return;
  }

  let status = footerStatus === "compact"
    ? \`MCP \${connectedCount}/\${enabledCount}\`
    : \`\${enabledCount} \${enabledCount === 1 ? "server" : "servers"} enabled\`;
  if (footerStatus === "full") {
    if (connectedCount > 0) status += \` (\${connectedCount} connected)\`;
    if (disabledCount > 0) status += \` (\${disabledCount} disabled)\`;
  }`;
const statusAfter = `  if (footerStatus === "off" || (footerStatus === "problems" && connectedCount === enabledCount)) {
    ui.setStatus("mcp", undefined);
    return;
  }

  const disconnectedCount = enabledCount - connectedCount;
  let status = footerStatus === "compact"
    ? \`MCP \${connectedCount}/\${enabledCount}\`
    : footerStatus === "problems"
      ? \`\${disconnectedCount} \${disconnectedCount === 1 ? "server" : "servers"} disconnected\`
      : \`\${enabledCount} \${enabledCount === 1 ? "server" : "servers"} enabled\`;
  if (footerStatus === "full") {
    if (connectedCount > 0) status += \` (\${connectedCount} connected)\`;
    if (disabledCount > 0) status += \` (\${disabledCount} disabled)\`;
  }`;
if (initChanged) {
	if (!next.includes(statusBefore)) {
		throw new Error(`anchor not found in ${initPath}; refresh apply-mcp-problems-status.mjs`);
	}
	next = next.replace(statusBefore, statusAfter);
	fs.writeFileSync(initPath, next);
}

console.log(
	typesChanged || initChanged
		? "[apply-mcp-problems-status] patched"
		: "[apply-mcp-problems-status] already patched",
);
