#!/usr/bin/env node
// pi-mcp-adapter's footer is either always visible or always hidden. Add a
// problems-only mode: healthy, cached, and intentionally idle lazy connections
// stay quiet, while an actual connection failure or auth problem remains visible.
// Also suppress the healthy startup toast in this mode; per-server failures keep
// their existing error notifications.

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
if (next.includes(startupBefore)) {
	next = next.replace(startupBefore, startupAfter);
	initChanged = true;
} else if (!next.includes(startupAfter)) {
	throw new Error(`startup anchor not found in ${initPath}; refresh apply-mcp-problems-status.mjs`);
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
const legacyStatusAfter = `  if (footerStatus === "off" || (footerStatus === "problems" && connectedCount === enabledCount)) {
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
const statusAfter = `  const problemCount = entries.filter(([name, definition]) => {
    if (isServerDisabled(definition)) return false;
    const connection = state.manager.getConnection(name);
    return getFailureAgeSeconds(state, name) !== null || connection?.status === "needs-auth";
  }).length;
  if (footerStatus === "off" || (footerStatus === "problems" && problemCount === 0)) {
    ui.setStatus("mcp", undefined);
    return;
  }

  let status = footerStatus === "compact"
    ? \`MCP \${connectedCount}/\${enabledCount}\`
    : footerStatus === "problems"
      ? \`\${problemCount} \${problemCount === 1 ? "server problem" : "server problems"}\`
      : \`\${enabledCount} \${enabledCount === 1 ? "server" : "servers"} enabled\`;
  if (footerStatus === "full") {
    if (connectedCount > 0) status += \` (\${connectedCount} connected)\`;
    if (disabledCount > 0) status += \` (\${disabledCount} disabled)\`;
  }`;
if (next.includes(statusBefore)) {
	next = next.replace(statusBefore, statusAfter);
	initChanged = true;
} else if (next.includes(legacyStatusAfter)) {
	next = next.replace(legacyStatusAfter, statusAfter);
	initChanged = true;
} else if (!next.includes(statusAfter)) {
	throw new Error(`status anchor not found in ${initPath}; refresh apply-mcp-problems-status.mjs`);
}
if (initChanged) fs.writeFileSync(initPath, next);

console.log(
	typesChanged || initChanged
		? "[apply-mcp-problems-status] patched"
		: "[apply-mcp-problems-status] already patched",
);
