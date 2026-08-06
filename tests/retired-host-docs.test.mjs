// DX finding 8: docs must never describe a retired surface as if it were
// live. docs/README.md's index used to describe host-mode.md as "Phase 1
// built, gated off by default" -- true of the doc's HISTORY, false of the
// current binary (host mode was deleted outright, not merely gated off; see
// AGENTS.md safety invariant #9). docs/MIGRATION.md's backup/restore row
// used to send an upgrader to `pix-host backup`/`pix-host restore`, which
// pix-host itself retired again to `pix-host memory snapshot|restore PATH`
// -- a second dead end in the docs, not just the CLI (see
// services/host/cmd/pix/retired_test.go's
// TestBackupRestore_ResolveDirectlyToMemorySnapshot for the CLI half).
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const readme = fs.readFileSync(path.join(repoRoot, "docs", "README.md"), "utf8");
const migration = fs.readFileSync(path.join(repoRoot, "docs", "MIGRATION.md"), "utf8");
const topReadme = fs.readFileSync(path.join(repoRoot, "README.md"), "utf8");

test("docs index marks host-mode.md as superseded/retired, not a live gated feature", () => {
	const line = readme.split("\n").find((l) => l.includes("host-mode.md"));
	assert.ok(line, "docs/README.md must still index host-mode.md");
	assert.match(line, /SUPERSEDED|RETIRED/i);
	assert.doesNotMatch(line, /gated off by default/i);
});

test("MIGRATION.md points backup/restore at the live memory snapshot commands directly", () => {
	assert.match(migration, /pix-host memory snapshot PATH.*pix-host memory restore PATH/);
	assert.doesNotMatch(migration, /`pix-host backup`\s*\/\s*`pix-host restore`/);
});

// `pix-host slack` (the local-stdio subcommand) is ITSELF retired
// (services/host/retired.go), so pointing a Slack migration/reference reader
// at `pix mcp register` — the path for a LOCAL stdio server — is a second
// dead end: `pix mcp register` cannot register a container by manifest.
test("MIGRATION.md sends Slack to the manifest path, not the dead pix-host subcommand", () => {
	const slackLine = migration.split("\n").find((l) => l.includes("`pix slack`"));
	assert.ok(slackLine, "MIGRATION.md must still document the retired `pix slack` verb");
	assert.match(slackLine, /--local --url <manifest>/);
	assert.doesNotMatch(slackLine, /`pix mcp register`/);
});

test("README.md's retired-surfaces section registers Slack by manifest, not `pix mcp register`", () => {
	const section = topReadme.slice(topReadme.indexOf("## Retired surfaces"));
	assert.match(section, /sbx mcp add slack --local --url <manifest>/);
	assert.match(section, /ships no built-in Slack MCP server/);
});
