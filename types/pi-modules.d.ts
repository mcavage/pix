// Ambient shims for the pi runtime packages.
//
// pi loads extensions with jiti and resolves these from its OWN node_modules at
// runtime (verified), but the pix repo has no local install of them, so
// tsserver/pi-lens would report "Cannot find module". These declarations make
// the editor resolve the imports (typed loosely as any) without adding real
// dependencies. They do NOT affect runtime — jiti uses the real modules.
//
// Per AGENTS.md, ambient types live in types/ (NEVER in extensions/, where pi
// would try to load a .d.ts as an extension factory and crash startup).

declare module "@earendil-works/pi-coding-agent" {
	export type ExtensionAPI = any;
	export type ExtensionContext = any;
	export const CONFIG_DIR_NAME: string;
	export function getAgentDir(): string;
	export function getPackageDir(): string;
	export function getMarkdownTheme(): any;
	export function parseFrontmatter<T = Record<string, string>>(
		content: string,
	): { frontmatter: T; body: string };
	export function withFileMutationQueue<T>(path: string, fn: () => Promise<T>): Promise<T>;
	const _default: any;
	export default _default;
}

declare module "@earendil-works/pi-tui" {
	export const Container: any;
	export const Text: any;
	export const Markdown: any;
	export const Spacer: any;
	const _default: any;
	export default _default;
}

declare module "@earendil-works/pi-ai" {
	export function StringEnum(values: readonly string[], options?: any): any;
	export type Message = any;
	const _default: any;
	export default _default;
}

declare module "typebox" {
	export const Type: any;
	const _default: any;
	export default _default;
}
