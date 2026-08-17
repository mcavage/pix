// W0 pin: the top-level ~/.config/pix/config.toml key surface (AGENTS.md's
// config.toml row: "Declares `services`, `mcp`,
// and the Ollama model names"). config.toml is managed exclusively through
// `pix config set`/`unset` (safety invariant #1) — the Go struct's `toml:"…"`
// tags ARE the on-disk contract, so a rename here is a silent breaking change
// for every user's existing file (an unknown key becomes `unknownKeys`, a
// dropped default becomes an unexplained behavior change) with no compiler or
// test to catch it, since Go/TOML round-trip fine either way.
export default [
	{
		id: "config-keys.top-level.surface",
		description: "Config's top-level toml tags, from the Services field through GogAccount, still name exactly this key set.",
		checks: [
			{
				file: "services/host/config/config.go",
				kind: "set",
				// Bounded to the AGENTS.md-documented user surface: Services through
				// GogAccount (KnowledgeBundles was retired, W2 U03A, along with the
				// built-in knowledge service — it no longer has a live on-disk key;
				// Slack went the same way in W2/U02a with the built-in Slack MCP
				// server, and GoogleWorkspaceAccess in W2/U02B with the built-in
				// Google Workspace wizard). Kits/Skills/Packs/Plugins/Host (further down
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
				// scripts/semantic-diff/intended-changes.json entry.
				//
				// google_workspace_account then followed it out. Pix ships no MCP
				// servers and names no vendor: Google Workspace is an ordinary
				// pack-declared server, and a per-user value like an account email
				// travels as that integration's env_keys in op-refs.env. A core
				// config key for one vendor's account was the last piece of the
				// special case — see docs/design/integrations-remediation.md.
				expected: [
					"services",
					"mcp",
					"memory_watcher_model",
					"memory_embed_model",
					"memory_capture",
					"ollama_bridge_model",
					"run_intent",
					"inference",
				],
			},
		],
	},
];
