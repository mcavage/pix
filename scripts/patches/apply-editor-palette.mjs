#!/usr/bin/env node
// Keep the input frame in the selected theme's accent. Thinking effort is
// already named in the footer; it is not an error/severity color scale.
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

export function patchEditorPalette(root) {
 const target = path.join(root, "dist/modes/interactive/theme/theme.js");
 const marker = "// Pix consistent input border";
 const source = fs.readFileSync(target, "utf8");
 if (source.includes(marker)) return;
 const before = `    getThinkingBorderColor(level) {
        // Map thinking levels to dedicated theme colors
        switch (level) {
            case "off":
                return (str) => this.fg("thinkingOff", str);
            case "minimal":
                return (str) => this.fg("thinkingMinimal", str);
            case "low":
                return (str) => this.fg("thinkingLow", str);
            case "medium":
                return (str) => this.fg("thinkingMedium", str);
            case "high":
                return (str) => this.fg("thinkingHigh", str);
            case "xhigh":
                return (str) => this.fg("thinkingXhigh", str);
            case "max":
                return (str) => this.fg("thinkingMax", str);
            default:
                return (str) => this.fg("thinkingOff", str);
        }
    }`;
 const after = `    getThinkingBorderColor(_level) {
        ${marker}
        return (str) => this.fg("borderAccent", str);
    }`;
 if (!source.includes(before)) throw new Error(`Editor palette anchor missing: ${target}`);
 fs.writeFileSync(target, source.replace(before, after));
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
 const root = process.env.PI_THEME_ROOT ?? path.join(execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim(), "@earendil-works/pi-coding-agent");
 patchEditorPalette(root);
 console.log("[apply-editor-palette] patched (or already patched)");
}
