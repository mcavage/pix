// Test stub for @earendil-works/pi-coding-agent (see tests/stub-loader.mjs).
export const CONFIG_DIR_NAME = ".pi";
export function getAgentDir() {
	return process.env.PI_TEST_AGENT_DIR || "/nonexistent-pi-test-agent";
}
export function getMarkdownTheme() {
	return {};
}
export function getPackageDir() {
	throw new Error("stub: no package dir in tests");
}
// Minimal but REAL frontmatter parsing. The old stub returned {} unconditionally,
// which silently made every per-agent frontmatter behavior (model, intent,
// thinking, max_turns, idle_ms, wall_ms, web) untestable: a test could assert an
// override and pass while the extension never saw the key. Scalars only, which is
// all agents/*.md uses; a value is left as its raw trimmed string, exactly as the
// real parser's Record<string, string> contract implies.
export function parseFrontmatter(content) {
	const m = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/.exec(content);
	if (!m) return { frontmatter: {}, body: content };
	const frontmatter = {};
	for (const line of m[1].split(/\r?\n/)) {
		if (!line.trim() || line.trimStart().startsWith("#")) continue;
		const i = line.indexOf(":");
		if (i <= 0) continue;
		const key = line.slice(0, i).trim();
		let value = line.slice(i + 1).trim();
		if (
			(value.startsWith('"') && value.endsWith('"') && value.length > 1) ||
			(value.startsWith("'") && value.endsWith("'") && value.length > 1)
		)
			value = value.slice(1, -1);
		frontmatter[key] = value;
	}
	return { frontmatter, body: content.slice(m[0].length) };
}
export async function withFileMutationQueue(_path, fn) {
	return fn();
}
