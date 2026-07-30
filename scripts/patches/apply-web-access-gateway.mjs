#!/usr/bin/env node
// pi-web-access 0.13.0 supports OpenAI-native search, but hardcodes both the
// public endpoint and model. Pix packs already own private inference topology;
// these generic config fields let a pack reuse that Responses backend without
// leaking its URL into the public image.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const root = process.env.PI_WEB_ACCESS_DIR ||
	path.join(os.homedir(), ".pi", "agent", "npm", "node_modules", "pi-web-access");
const sourcePath = path.join(root, "openai-search.ts");

function patch(before, after) {
	const current = fs.readFileSync(sourcePath, "utf8");
	if (current.includes(after)) return false;
	if (!current.includes(before)) {
		throw new Error(`anchor not found in ${sourcePath}; refresh apply-web-access-gateway.mjs`);
	}
	fs.writeFileSync(sourcePath, current.replace(before, after));
	return true;
}

let changed = false;
changed = patch(
	`interface WebSearchConfig {\n\topenaiApiKey?: unknown;\n}`,
	`interface WebSearchConfig {\n\topenaiApiKey?: unknown;\n\topenaiApiKeyEnv?: unknown;\n\topenaiBaseUrl?: unknown;\n\topenaiModel?: unknown;\n}`,
) || changed;

changed = patch(
	`\tconst apiKey = normalizeApiKey(process.env.OPENAI_API_KEY) ?? normalizeApiKey(loadConfig().openaiApiKey);\n\treturn apiKey\n\t\t? { provider: "openai", apiKey, model: "gpt-5.4", headers: {} }\n\t\t: undefined;`,
	`\tconst config = loadConfig();\n\tconst configuredEnv = normalizeApiKey(config.openaiApiKeyEnv);\n\tif (configuredEnv && !/^[A-Z_][A-Z0-9_]*$/.test(configuredEnv)) {\n\t\tthrow new Error("openaiApiKeyEnv must be an uppercase environment-variable name");\n\t}\n\tconst apiKey = normalizeApiKey(configuredEnv ? process.env[configuredEnv] : undefined)\n\t\t?? normalizeApiKey(process.env.OPENAI_API_KEY)\n\t\t?? normalizeApiKey(config.openaiApiKey);\n\tconst model = normalizeApiKey(config.openaiModel) ?? "gpt-5.4";\n\treturn apiKey\n\t\t? { provider: "openai", apiKey, model, headers: {} }\n\t\t: undefined;`,
) || changed;

changed = patch(
	`export async function isOpenAISearchAvailable(ctx?: ExtensionContext): Promise<boolean> {\n\treturn !!(await resolveOpenAIAuth(ctx));\n}`,
	`export async function isOpenAISearchAvailable(ctx?: ExtensionContext): Promise<boolean> {\n\treturn !!(await resolveOpenAIAuth(ctx));\n}\n\nfunction configuredResponsesURL(): string {\n\tconst configured = normalizeApiKey(loadConfig().openaiBaseUrl);\n\tif (!configured) return OPENAI_RESPONSES_URL;\n\tconst url = new URL(configured);\n\tconst loopback = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "::1";\n\tif (url.protocol !== "https:" && !(url.protocol === "http:" && loopback)) {\n\t\tthrow new Error("openaiBaseUrl must use HTTPS (or loopback HTTP)");\n\t}\n\turl.search = "";\n\turl.hash = "";\n\turl.pathname = url.pathname.replace(/\\\/$/, "") + (url.pathname.endsWith("/responses") ? "" : "/responses");\n\treturn url.toString();\n}`,
) || changed;

changed = patch(
	`\t\tconst response = await fetch(useCodexEndpoint ? CODEX_RESPONSES_URL : OPENAI_RESPONSES_URL, {`,
	`\t\tconst response = await fetch(useCodexEndpoint ? CODEX_RESPONSES_URL : configuredResponsesURL(), {`,
) || changed;

console.log(changed ? "[apply-web-access-gateway] patched" : "[apply-web-access-gateway] already patched");
