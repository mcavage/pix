import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const skill = fs.readFileSync(new URL("../skills/healthcheck/SKILL.md", import.meta.url), "utf8");

test("healthcheck never mistakes the memory service's default embed model for configured inference", () => {
	assert.match(skill, /embed_model.*not proof.*configured/is);
	assert.match(skill, /embed_healthy:false.*does not.*overall.*degraded/is);
});

test("healthcheck uses v2 MCP memory and no removed host-service advice", () => {
	assert.match(skill, /memory_status.*memory_stats.*memory_recall/s);
	assert.doesNotMatch(skill, /host\.docker\.internal:11435|make serve|pix serve|active pack|pack-provided/i);
});
