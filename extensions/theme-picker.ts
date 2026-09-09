// Pix theme browser: preview a palette sample with arrow keys and commit with Enter.
//
// Preview rendering deliberately does not switch Pi's live Theme instance. The
// public extension API cannot restore an automatic light/dark selection after an
// in-memory switch. Passing a theme name only on Enter preserves cancel semantics.
// Pix also stores the committed name in personal context so the next disposable
// sandbox can start with the same theme.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { DynamicBorder } from "@earendil-works/pi-coding-agent";
import { Container, type SelectItem, SelectList, Text } from "@earendil-works/pi-tui";
import { findPersonalContextDir } from "../lib/output-style.ts";
import { writeThemePreference } from "../lib/theme-preference.ts";

interface CatalogEntry {
	label: string;
	appearance: "dark" | "light";
	description: string;
}

const CATALOG: Record<string, CatalogEntry> = {
	pix: { label: "Pix", appearance: "dark", description: "Cool blue, teal, and amber" },
	dracula: { label: "Dracula", appearance: "dark", description: "Violet and cyan on charcoal" },
	"catppuccin-mocha": { label: "Catppuccin Mocha", appearance: "dark", description: "Soft pastels on deep blue" },
	"catppuccin-latte": { label: "Catppuccin Latte", appearance: "light", description: "Soft pastels on warm white" },
	"tokyo-night": { label: "Tokyo Night", appearance: "dark", description: "Blue and violet after dark" },
	nord: { label: "Nord", appearance: "dark", description: "Cool arctic blue" },
	"gruvbox-dark": { label: "Gruvbox Dark", appearance: "dark", description: "Warm retro earth tones" },
	"gruvbox-light": { label: "Gruvbox Light", appearance: "light", description: "Warm paper and earth tones" },
	"solarized-dark": { label: "Solarized Dark", appearance: "dark", description: "Low-contrast blue and amber" },
	"solarized-light": { label: "Solarized Light", appearance: "light", description: "Low-contrast cream and blue" },
	"one-dark": { label: "One Dark", appearance: "dark", description: "Crisp editor blues and greens" },
	"rose-pine": { label: "Rosé Pine", appearance: "dark", description: "Muted rose and gold" },
	"rose-pine-dawn": { label: "Rosé Pine Dawn", appearance: "light", description: "Muted rose on warm white" },
	kanagawa: { label: "Kanagawa", appearance: "dark", description: "Ink, wave blue, and autumn gold" },
	dark: { label: "Dark", appearance: "dark", description: "Neutral Pi dark palette" },
	light: { label: "Light", appearance: "light", description: "Neutral Pi light palette" },
};

const ORDER = Object.keys(CATALOG);
const HIDDEN_THEMES = new Set(["host"]); // Host mode's red palette is a safety cue, not a cosmetic choice.

interface ThemePickerOptions {
	contextDir?: string | null;
}

function customLabel(name: string): string {
	return name
		.split(/[-_.]+/)
		.filter(Boolean)
		.map((part) => part.charAt(0).toUpperCase() + part.slice(1))
		.join(" ");
}

export function registerThemePicker(pi: ExtensionAPI, options: ThemePickerOptions = {}): void {
	const contextDir = options.contextDir === undefined
		? findPersonalContextDir(process.argv, process.env)
		: options.contextDir;

	function availableThemes(ctx: any) {
		const loaded = ctx.ui
			.getAllThemes()
			.filter(({ name }: { name: string }) => Boolean(CATALOG[name]) && !HIDDEN_THEMES.has(name))
			.map(({ name }: { name: string }) => ({ name, theme: ctx.ui.getTheme(name) }))
			.filter(({ theme }: { theme: unknown }) => Boolean(theme));
		const rank = new Map(ORDER.map((name, index) => [name, index]));
		return loaded.sort((a: { name: string }, b: { name: string }) => {
			const ar = rank.get(a.name) ?? Number.MAX_SAFE_INTEGER;
			const br = rank.get(b.name) ?? Number.MAX_SAFE_INTEGER;
			return ar - br || a.name.localeCompare(b.name);
		});
	}

	function persist(name: string, ctx: any): void {
		if (!contextDir) {
			ctx.ui.notify("Theme changed for this sandbox. Personal context is unavailable, so it will not carry to the next one.", "warning");
			return;
		}
		try {
			writeThemePreference(contextDir, name);
		} catch (error) {
			ctx.ui.notify(`Theme changed, but Pix could not save it: ${error instanceof Error ? error.message : String(error)}`, "warning");
		}
	}

	function apply(name: string, ctx: any): boolean {
		const result = ctx.ui.setTheme(name);
		if (!result.success) {
			ctx.ui.notify(`Could not use theme "${name}": ${result.error ?? "unknown error"}`, "error");
			return false;
		}
		persist(name, ctx);
		ctx.ui.notify(`Theme: ${CATALOG[name]?.label ?? customLabel(name)}`, "info");
		return true;
	}

	async function browse(initialQuery: string, ctx: any): Promise<void> {
		if (ctx.mode !== "tui") {
			ctx.ui.notify("/theme requires interactive mode.", "error");
			return;
		}
		const loadedThemes = availableThemes(ctx);
		if (loadedThemes.length === 0) {
			ctx.ui.notify("No themes are available. Run /reload after restoring the Pix theme catalog.", "error");
			return;
		}
		const query = initialQuery.toLowerCase();
		const themes = query
			? loadedThemes.filter(({ name }: { name: string }) => `${name} ${CATALOG[name]?.label ?? ""}`.toLowerCase().includes(query))
			: loadedThemes;
		if (themes.length === 0) {
			ctx.ui.notify(`No themes match "${initialQuery}".`, "warning");
			return;
		}

		const originalName = ctx.ui.theme.name;
		const selected: string | null = await ctx.ui.custom((tui: any, theme: any, _keybindings: any, done: (value: string | null) => void) => {
			const items: SelectItem[] = themes.map(({ name }: { name: string }) => {
				const entry = CATALOG[name];
				return {
					value: name,
					label: `${entry?.label ?? customLabel(name)} [${entry?.appearance ?? "custom"}]${name === originalName ? " [current]" : ""}`,
					description: entry?.description ?? "Custom theme",
				};
			});
			const container = new Container();
			container.addChild(new DynamicBorder((text: string) => theme.fg("accent", text)));
			container.addChild(new Text(theme.fg("accent", theme.bold("Theme")), 1, 0));
			container.addChild(new Text(theme.fg("muted", "Moving the selection previews the palette below."), 1, 0));
			const list = new SelectList(items, Math.min(items.length, 12), {
				selectedPrefix: (text) => theme.fg("accent", text),
				selectedText: (text) => theme.fg("accent", text),
				description: (text) => theme.fg("muted", text),
				scrollInfo: (text) => theme.fg("dim", text),
				noMatch: (text) => theme.fg("warning", text),
			});
			list.onSelectionChange = () => {
				container.invalidate();
				tui.requestRender();
			};
			list.onSelect = (item) => done(item.value);
			list.onCancel = () => done(null);
			const currentIndex = items.findIndex((item) => item.value === originalName);
			if (currentIndex >= 0) list.setSelectedIndex(currentIndex);
			container.addChild(list);
			container.addChild({
				render(width: number) {
					const selectedName = list.getSelectedItem()?.value ?? originalName;
					const candidate = themes.find(({ name }: { name: string }) => name === selectedName)?.theme ?? theme;
					const sample = [
						candidate.fg("mdHeading", candidate.bold("Aa Heading")),
						candidate.fg("mdLink", "link"),
						candidate.fg("syntaxKeyword", "const"),
						candidate.fg("syntaxVariable", "answer"),
						candidate.fg("syntaxOperator", "="),
						candidate.fg("syntaxNumber", "42"),
						candidate.fg("success", "+ added"),
						candidate.fg("warning", "! warning"),
						candidate.fg("error", "× error"),
					].join("  ");
					return new Text(candidate.bg("userMessageBg", sample), 1, 0).render(width);
				},
				invalidate() {},
			});
			container.addChild(new Text(theme.fg("dim", "↑↓ preview  enter use  esc cancel  /theme QUERY filters"), 1, 0));
			container.addChild(new DynamicBorder((text: string) => theme.fg("accent", text)));
			return {
				render: (width: number) => container.render(width),
				invalidate: () => container.invalidate(),
				handleInput: (data: string) => {
					list.handleInput(data);
					tui.requestRender();
				},
			};
		});

		if (selected) apply(selected, ctx);
	}

	pi.registerCommand("theme", {
		description: "Browse, preview, and switch built-in themes",
		handler: async (args: string, ctx: any) => {
			const requested = args?.trim() ?? "";
			if (requested) {
				const exact = availableThemes(ctx).find(({ name }: { name: string }) => name.toLowerCase() === requested.toLowerCase());
				if (exact) {
					apply(exact.name, ctx);
					return;
				}
			}
			await browse(requested, ctx);
		},
	});
}

export default function themePicker(pi: ExtensionAPI): void {
	registerThemePicker(pi);
}
