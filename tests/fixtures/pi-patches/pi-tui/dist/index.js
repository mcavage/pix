// Fixture-only slim re-export: the real @earendil-works/pi-tui index.js also
// re-exports components/*, keybindings.js, autocomplete.js, etc. The
// tui-bottom-pin harness scripts (docs/upstream/tui-bottom-pin/*.mjs) only
// need TUI + CURSOR_MARKER, so this fixture trims the surface to what tui.js
// itself imports (keys.js, terminal-colors.js, terminal-image.js, utils.js,
// get-east-asian-width) instead of vendoring the entire package.
export { Container, CURSOR_MARKER, isFocusable, TUI } from "./tui.js";
