# Complete theme surfaces in Pi 0.85.1

Pi's JSON palettes define `export.pageBg`, but the live TUI only paints named
message/tool backgrounds. Editor text, ordinary prose, and blank cells use the
terminal defaults. A light palette on a dark terminal is consequently incomplete.

`apply-theme-surface.mjs` makes the resolved page background available on the
live Theme and supplies its foreground/background to the shared TUI line pass.
Both the main-screen and fullscreen renderers use this pass after overlays and
selection. It fills each row to the terminal width, maps default-color resets
back to the current palette, preserves explicit colors, and leaves image rows
alone. Each output row still ends with Pi's style reset. No OSC color changes
are sent, so the shell retains its original terminal colors.

The provider resolves the live Theme every frame: startup, `/theme`, settings,
and automatic theme changes all share the same behavior. Themes without an
explicit page background retain the terminal default. The patch checks all
anchors before writing and fails the image build on upstream drift.

The image points `pi` at `dist/cli.js`, not the upstream bundled CLI. This is
required: the bundle embeds separate copies of the renderer and theme singleton,
so patching the module files alone does not change the installed command.
Extensions and the interactive UI must share the same module runtime.

`apply-editor-palette.mjs` keeps the input border at `borderAccent` for every
thinking level. Effort remains visible as text in the footer; bash mode retains
its palette-specific cue. Pix help uses semantic text colors, and shipped
palettes define body foregrounds explicitly rather than inheriting the terminal.

Verification:

- `node --test tests/theme-surface.test.mjs` checks blank cells and nested SGR
  resets, including RGB operands that resemble reset codes.
- `PI_THEME_ROOT=/path/to/pi-coding-agent node scripts/test-theme-surface-runtime.mjs`
  patches the pinned package idempotently and drives its real Editor and both
  renderers through dark/light palette changes. Release patch smoke runs it too.
- Set `PIX_THEME_FRAMES` to save rendered ANSI rows for visual inspection.

`apply-theme-preview.mjs` exposes a non-persisting preview transaction through
Pi's extension UI. The picker runs as an overlay, keeping the real editor and
transcript visible. Arrow keys update the global theme and editor border;
Escape (or an error) restores the exact original Theme instance, file watcher,
and automatic-theme mode. Only Enter calls the normal saved-theme setter.
The installed-command PTY check covers preview before Enter, unchanged settings
during preview, Escape restoration, and committing on Enter.
