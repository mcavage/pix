#!/usr/bin/env node
// Expose a non-persisting preview transaction to Pix's theme picker. Capture the
// actual Theme instance, watcher, and automatic-mode state; do not rewrite settings.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export function patchThemePreview(root) {
 const themeDir = path.join(root, "dist/modes/interactive/theme");
 const changes = [
  [path.join(themeDir, "theme.js"), "export function setRegisteredThemes(themes) {", `export function captureThemePreview() {
    const original = globalThis[THEME_KEY];
    const name = currentThemeName;
    const watching = Boolean(themeWatcher);
    return () => {
        stopThemeWatcher();
        currentThemeName = name;
        setGlobalTheme(original);
        if (watching) startThemeWatcher();
        onThemeChangeCallback?.();
    };
}
export function setRegisteredThemes(themes) {`],
  [path.join(themeDir, "theme-controller.js"), "    preview(themeSettingOrName) {", `    beginPreview() {
        const restoreTheme = captureThemePreview();
        const autoSync = this.autoSyncEnabled;
        this.setAutoSync(false);
        let restored = false;
        return {
            preview: (name) => {
                if (restored) return { success: false, error: "Preview is closed" };
                const result = setTheme(name, true);
                this.notifyChanged();
                this.ui.requestRender();
                return result;
            },
            restore: () => {
                if (restored) return;
                restored = true;
                restoreTheme();
                this.setAutoSync(autoSync);
                this.notifyChanged();
                this.ui.requestRender();
            },
        };
    }
    preview(themeSettingOrName) {`],
  [path.join(root, "dist/modes/interactive/interactive-mode.js"), "            getAllThemes: () => getAvailableThemesWithPaths(),", "            beginThemePreview: () => this.themeController.beginPreview(),\n            getAllThemes: () => getAvailableThemesWithPaths(),"],
 ];
 const patched = changes.map(([file, before, after]) => {
  let source = fs.readFileSync(file, "utf8");
  if (source.includes("// Pix live theme preview")) return [file, null];
  if (!source.includes(before)) throw new Error(`Theme preview anchor missing: ${file}`);
  source = source.replace(before, after);
  if (file.endsWith("theme-controller.js")) source = `import { captureThemePreview } from "./theme.js";\n${source}`;
  return [file, `// Pix live theme preview\n${source}`];
 });
 for (const [file, source] of patched) if (source !== null) fs.writeFileSync(file, source);
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
 const root = process.env.PI_THEME_ROOT ?? path.join(execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim(), "@earendil-works/pi-coding-agent");
 patchThemePreview(root);
 console.log("[apply-theme-preview] patched (or already patched)");
}
