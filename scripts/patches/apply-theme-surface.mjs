#!/usr/bin/env node
// Pi 0.85.1: paint the complete TUI with the palette, including editor and
// blank cells. Use SGR per frame, never OSC terminal-global color mutations.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

export function patchThemeSurface(root) {
 const targets = [
  [join(root, "dist/modes/interactive/theme/theme.js"), [
   ['        const backgrounds = {\n            ...bgColors,', '        const backgrounds = {\n            appBg: "",\n            ...bgColors,'],
   ['    const bgColors = {};', '    const bgColors = { appBg: resolveVarRefs(themeJson.export?.pageBg ?? "", themeJson.vars ?? {}) };'],
  ]],
  [join(root, "dist/modes/interactive/interactive-mode.js"), [
   ['        this.ui.start();', '        // Pix theme surface: resolve the live theme on every frame.\n        this.ui.pixThemeSurface = () => ({ fg: theme.getFgAnsi("text"), bg: theme.getBgAnsi("appBg") });\n        this.ui.start();'],
  ]],
  [join(root, "node_modules/@earendil-works/pi-tui/dist/tui.js"), [
   ['                lines[i] = normalizeTerminalOutput(line) + reset;', '                const surface = this.pixThemeSurface?.();\n                lines[i] = (surface ? pixThemeSurface(normalizeTerminalOutput(line), this.terminal.columns, surface) : normalizeTerminalOutput(line)) + reset;'],
  ]],
 ];
 const block = readFileSync(new URL("./theme-surface.block.txt", import.meta.url), "utf8");
 // Validate all anchors before writing any file; fail the build on upstream drift.
 const changes = targets.map(([file, edits]) => {
  let source = readFileSync(file, "utf8");
  if (source.includes("// Pix theme surface patch")) return [file, null];
  for (const [before, after] of edits) {
   if (!source.includes(before)) throw new Error(`Theme surface patch anchor missing: ${file}: ${before}`);
   source = source.replace(before, after);
  }
  if (file.endsWith("/pi-tui/dist/tui.js")) source += `\n${block}`;
  return [file, `// Pix theme surface patch\n${source}`];
 });
 for (const [file, source] of changes) if (source !== null) writeFileSync(file, source);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
 const root = process.env.PI_THEME_ROOT ?? join(execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim(), "@earendil-works/pi-coding-agent");
 patchThemeSurface(root);
 console.log("[apply-theme-surface] patched (or already patched)");
}
