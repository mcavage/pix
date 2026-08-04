// W0 pins: on-disk permission modes. The distinction that matters is which
// files are SECRET-BEARING (0600, plus a 0700 parent dir) vs. plain config
// (0644) — a mode that quietly widened or tightened would be invisible to
// every functional test (the file still reads and writes fine either way).
export default [
	{
		id: "permissions.config-toml.seed-mode",
		description: "Seed() creates ~/.config/pix (0700) and writes config.toml at 0644 — it is NOT secret-bearing (op:// refs only, never resolved values).",
		checks: [
			{
				// Seed() is the last function in config.go at W0 (no trailing anchor to
				// bound against) — the region runs to end of file, which is fine: there
				// is nothing after it to leak into.
				file: "services/host/config/config.go",
				kind: "contains",
				region: { start: "func Seed(path string) (bool, error) {" },
				values: ["os.MkdirAll(filepath.Dir(path), 0o700)", "os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)"],
			},
			{
				file: "services/host/config/config.go",
				kind: "notContains",
				region: { start: "func Seed(path string) (bool, error) {" },
				values: ["0o600"],
			},
		],
	},
	{
		id: "permissions.op-refs.seed-mode",
		description: "SeedOpRefsAt creates the same 0700 config dir but writes op-refs.env at 0600 — it is where op:// refs live, tightened even if the parent dir was created 0755 elsewhere.",
		checks: [
			{
				file: "services/host/config/config.go",
				kind: "contains",
				region: { start: "func SeedOpRefsAt(path string) (created bool, err error) {", end: "\n\n// Seed writes" },
				values: ["os.MkdirAll(filepath.Dir(path), 0o700)", "os.Chmod(filepath.Dir(path), 0o700)", "os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)"],
			},
			{
				file: "services/host/config/config.go",
				kind: "notContains",
				region: { start: "func SeedOpRefsAt(path string) (created bool, err error) {", end: "\n\n// Seed writes" },
				values: ["0o644"],
			},
		],
	},
	{
		id: "permissions.secret-write.mode",
		description: "pix secret set writes op-refs.env content back at 0600 (secret.go's Set path), never a looser mode.",
		checks: [
			{
				file: "services/host/secret/secret.go",
				kind: "contains",
				values: ["env.WriteFile(path, []byte(newContent), 0o600)"],
			},
		],
	},
];
