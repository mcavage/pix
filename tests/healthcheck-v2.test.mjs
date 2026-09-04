import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const skill = fs.readFileSync(new URL("../skills/healthcheck/SKILL.md", import.meta.url), "utf8");

test("healthcheck never mistakes the memory service's default embed model for configured inference", () => {
	assert.match(skill, /embed_model.*not proof.*configured/is);
	assert.match(skill, /embed_healthy:false.*does not.*overall.*degraded/is);
});

test("healthcheck does not invent native-provider roster degradation", () => {
	assert.match(skill, /NEVER mark.*inference\.json.*degraded/is);
	assert.match(skill, /NEVER recommend.*inference roster.*native provider/is);
	assert.match(skill, /same vendor.*not.*degraded/is);
});

test("healthcheck rejects unexpected or retired subagent models", () => {
	assert.match(skill, /retired model.*FAIL/is);
	assert.match(skill, /no\s+explicit agent model or environment roster.*different.*parent.*FAIL/is);
});

test("healthcheck uses v2 MCP memory and no removed host-service advice", () => {
	assert.match(skill, /memory_status.*memory_stats.*memory_recall/s);
	assert.doesNotMatch(skill, /host\.docker\.internal:11435|make serve|pix serve|active pack|pack-provided/i);
});
