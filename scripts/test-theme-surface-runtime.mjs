// Run against the pinned, installed Pi package; also used by release patch smoke.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { patchThemeSurface } from "./patches/apply-theme-surface.mjs";
import { patchEditorPalette } from "./patches/apply-editor-palette.mjs";
import { patchThemePreview } from "./patches/apply-theme-preview.mjs";
const root = process.env.PI_THEME_ROOT ?? path.join(execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim(), "@earendil-works/pi-coding-agent");
patchThemeSurface(root);
patchThemeSurface(root);
patchEditorPalette(root);
patchEditorPalette(root);
patchThemePreview(root);
patchThemePreview(root);
const load = (file) => import(pathToFileURL(path.join(root, file)).href);
const themes = await load("dist/modes/interactive/theme/theme.js");
const { Editor } = await load("node_modules/@earendil-works/pi-tui/dist/components/editor.js");
const { visibleWidth } = await load("node_modules/@earendil-works/pi-tui/dist/utils.js");
const frames = {};
for (const [file, name] of [["tui-main-screen", "TuiMainScreen"], ["tui-alt-screen", "TuiAltScreen"]]) {
 const { [name]: Renderer } = await load(`node_modules/@earendil-works/pi-tui/dist/${file}.js`);
 const terminal = { columns: 80, rows: 24, write() {}, hideCursor() {}, showCursor() {} };
 const tui = new Renderer(terminal, false);
 tui.pixThemeSurface = () => ({ fg: themes.theme.getFgAnsi("text"), bg: themes.theme.getBgAnsi("appBg") });
 let content = [];
 tui.addChild({ render: () => content, invalidate() {} });
 if (name === "TuiAltScreen") tui.altScreenActive = true;
 const editor = new Editor(tui, themes.getEditorTheme());
 editor.setText("Explain this code and help me fix it.");
 for (const palette of ["dracula", "catppuccin-latte", "nord"]) {
  const theme = themes.loadThemeFromPath(path.resolve("themes", `${palette}.json`), "truecolor");
  themes.setThemeInstance(theme);
  for (const level of ["off", "minimal", "low", "medium", "high", "xhigh", "max"]) {
   assert.equal(theme.getThinkingBorderColor(level)("border"), theme.fg("borderAccent", "border"));
  }
  assert.equal(theme.getBashModeBorderColor()("border"), theme.fg("bashMode", "border"));
  editor.borderColor = theme.getThinkingBorderColor("medium");
  content = [
   theme.fg("accent", "Pix · Theme preview"), "",
   "Assistant text uses the palette foreground.",
   theme.bg("userMessageBg", theme.fg("userMessageText", "User message")),
   theme.bg("toolSuccessBg", theme.fg("toolOutput", "✓ Tool completed")),
   "", ...editor.render(80), "", "model · context · cost", "",
  ];
  tui.doRender();
  const lines = name === "TuiAltScreen" ? tui.previousScreen : tui.previousLines;
  if (name === "TuiAltScreen") assert.equal(lines.length, terminal.rows);
  for (const line of lines) {
   assert.equal(visibleWidth(line), 80);
   assert.ok(line.startsWith(theme.getFgAnsi("text") + theme.getBgAnsi("appBg")));
   assert.ok(line.endsWith("\x1b[0m\x1b]8;;\x07") || line.includes("\x1b[0m"), "reset style after each row");
  }
  frames[`${name}-${palette}`] = lines;
 }
}
if (process.env.PIX_THEME_FRAMES) fs.writeFileSync(process.env.PIX_THEME_FRAMES, JSON.stringify(frames));
console.log("Theme surface: real Pi editor, main/fullscreen renderers, dark/light switching passed.");

// Preview must restore the exact theme and automatic selection, without a save.
const { InteractiveThemeController } = await load("dist/modes/interactive/theme/theme-controller.js");
themes.setRegisteredThemes(fs.readdirSync("themes").filter((f) => f.endsWith(".json")).map((f) => ({ name: f.slice(0, -5), sourcePath: path.resolve("themes", f) })));
let saved = 0;
const controller = new InteractiveThemeController({
 invalidate() {}, requestRender() {}, setTerminalColorSchemeNotifications() {},
 onTerminalColorSchemeChange() { return () => {}; },
}, {
 initialThemeSetting: "light/dark",
 getSettingsManager: () => ({ getThemeSetting: () => "light/dark", setTheme: () => saved++ }),
 showError(message) { throw new Error(message); }, onChanged() {},
});
controller.setAutoSync(true);
const originalName = themes.theme.name;
const preview = controller.beginPreview();
assert.equal(preview.preview("catppuccin-latte").success, true);
assert.equal(themes.theme.name, "catppuccin-latte");
assert.equal(controller.getThemeSelection(), "light/dark");
preview.restore();
preview.restore();
assert.equal(themes.theme.name, originalName);
assert.equal(controller.autoSyncEnabled, true);
assert.equal(saved, 0);
controller.dispose();
themes.stopThemeWatcher();
console.log("Theme preview: cancellation restores appearance and automatic mode without saving.");
