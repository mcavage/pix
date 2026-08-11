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
export function parseFrontmatter(content) {
	return { frontmatter: {}, body: content };
}
export async function withFileMutationQueue(_path, fn) {
	return fn();
}
