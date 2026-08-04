// W0 pin: the top-level ~/.config/pix/config.toml key surface (AGENTS.md's
// config.toml row: "Declares `services`, `mcp`, `google_workspace_account`,
// and the Ollama model names"). config.toml is managed exclusively through
// `pix config set`/`unset` (safety invariant #1) — the Go struct's `toml:"…"`
// tags ARE the on-disk contract, so a rename here is a silent breaking change
// for every user's existing file (an unknown key becomes `unknownKeys`, a
// dropped default becomes an unexplained behavior change) with no compiler or
// test to catch it, since Go/TOML round-trip fine either way.
export default [
	{
		id: "config-keys.top-level.surface",
		description: "Config's top-level toml tags, from the Services field through KnowledgeBundles, still name exactly this key set.",
		checks: [
			{
				file: "services/host/config/config.go",
				kind: "set",
				// Bounded to the AGENTS.md-documented user surface: Services through
				// KnowledgeBundles. Kits/Skills/Packs/Plugins/Host/Slack (further down
				// the same struct) are real keys too but are out of scope for this W0
				// pin — Story-scoped follow-up, not this guard's job to enumerate the
				// entire struct.
				region: { start: 'Services []string `toml:"-"`', end: "\n\tKits struct {" },
				pattern: 'toml:"([^",]+)',
				// "-" (Services' own tag, deliberately un-serialized — ServicesRaw is
				// the TOML-facing field) sits ON the start-anchor line itself and is
				// excluded from the scanned region by construction; it is not a real
				// on-disk key so it does not belong in this set anyway.
				// google_workspace_access dropped W2 U02B: it was the runtime profile
				// ("create-docs") the built-in `pix gworkspace setup --create-docs`
				// wizard wrote for the separate google-docs-create MCP. Both the
				// wizard and that MCP are retired — see
				// docs/design/gworkspace-externalization.md and the matching
				// scripts/semantic-diff/intended-changes.json entry. google_workspace_account
				// stays: gog is still registered generically via `pix mcp register`.
				expected: [
					"services",
					"mcp",
					"memory_watcher_model",
					"memory_embed_model",
					"ollama_bridge_model",
					"run_intent",
					"inference",
					"google_workspace_account",
					"knowledge_bundles",
				],
			},
		],
	},
];
