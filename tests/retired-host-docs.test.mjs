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
