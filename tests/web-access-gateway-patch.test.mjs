import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

test("pinned pi-web-access patch adds gateway seams and preserves pack provider policy", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-web-access-patch-"));
	const source = `
const OPENAI_RESPONSES_URL = "https://api.openai.com/v1/responses";
const CODEX_RESPONSES_URL = "https://chatgpt.com/backend-api/codex/responses";
interface WebSearchConfig {
	openaiApiKey?: unknown;
}
function normalizeApiKey(value: unknown): string | null { return typeof value === "string" ? value : null; }
function loadConfig(): WebSearchConfig { return {}; }
export async function resolveOpenAIAuth(ctx?: unknown) {
	const apiKey = normalizeApiKey(process.env.OPENAI_API_KEY) ?? normalizeApiKey(loadConfig().openaiApiKey);
	return apiKey
		? { provider: "openai", apiKey, model: "gpt-5.4", headers: {} }
		: undefined;
}
export async function isOpenAISearchAvailable(ctx?: ExtensionContext): Promise<boolean> {
	return !!(await resolveOpenAIAuth(ctx));
}
async function run(useCodexEndpoint: boolean) {
		const response = await fetch(useCodexEndpoint ? CODEX_RESPONSES_URL : OPENAI_RESPONSES_URL, {
			method: "POST",
		});
}
`;
	fs.writeFileSync(path.join(dir, "openai-search.ts"), source);
	fs.writeFileSync(path.join(dir, "index.ts"), `
type SearchProvider = "auto" | "openai" | "exa";
function loadConfig(): { provider?: string } { return {}; }
function normalizeProviderInput(value: unknown): SearchProvider | undefined {
	if (value === undefined) return undefined;
	return value as SearchProvider;
}
function normalizeCuratorTimeoutSeconds(value: unknown): number | undefined { return undefined; }
function resolveProvider(requested: unknown) {
	const provider = normalizeProviderInput(requested ?? loadConfig().provider ?? "auto") ?? "auto";
	return provider;
}
function curated(params: { provider?: string }) {
				const rawSearchProvider = normalizeProviderInput(params.provider ?? loadConfig().provider ?? "auto") ?? "auto";
	return rawSearchProvider;
}
function headless(params: { provider?: string }) {
			const resolvedProvider = normalizeProviderInput(params.provider ?? loadConfig().provider);
	return resolvedProvider;
}
`);
	const script = path.join(repo, "scripts", "patches", "apply-web-access-gateway.mjs");
	const env = { ...process.env, PI_WEB_ACCESS_DIR: dir };
	const first = execFileSync(process.execPath, [script], { env, encoding: "utf8" });
	const second = execFileSync(process.execPath, [script], { env, encoding: "utf8" });
	assert.match(first, /patched/);
	assert.match(second, /already patched/);
	const patched = fs.readFileSync(path.join(dir, "openai-search.ts"), "utf8");
	for (const expected of ["openaiApiKeyEnv", "openaiBaseUrl", "openaiModel", "configuredResponsesURL()"])
		assert.match(patched, new RegExp(expected.replace(/[()]/g, "\\$&")));
	assert.match(patched, /fetch\(useCodexEndpoint \? CODEX_RESPONSES_URL : configuredResponsesURL\(\)/);
	const patchedIndex = fs.readFileSync(path.join(dir, "index.ts"), "utf8");
	assert.match(patchedIndex, /function resolveConfiguredProviderInput/);
	assert.equal((patchedIndex.match(/resolveConfiguredProviderInput\(params\.provider\)/g) ?? []).length, 2);
	assert.match(patchedIndex, /resolveConfiguredProviderInput\(requested\)/);
});
