// W0 pins: on-disk permission modes. (The config.toml Seed() pin was dropped
// with the dead seeder itself — nothing in the tree wrote a template
// config.toml any more; Save() owns the file, at 0600, and is pinned by its own
// tests.)
//  The distinction that matters is which
// files are SECRET-BEARING (0600, plus a 0700 parent dir) vs. plain config
// (0644) — a mode that quietly widened or tightened would be invisible to
// every functional test (the file still reads and writes fine either way).
export default [
	{
		id: "permissions.op-refs.seed-mode",
		description: "SeedOpRefsAt creates the same 0700 config dir but writes op-refs.env at 0600 — it is where op:// refs live, tightened even if the parent dir was created 0755 elsewhere.",
		checks: [
			{
				file: "services/host/config/config.go",
				kind: "contains",
				region: { start: "func SeedOpRefsAt(path string) (created bool, err error) {" },
				values: ["os.MkdirAll(filepath.Dir(path), 0o700)", "os.Chmod(filepath.Dir(path), 0o700)", "os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)"],
			},
			{
				file: "services/host/config/config.go",
				kind: "notContains",
				region: { start: "func SeedOpRefsAt(path string) (created bool, err error) {" },
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
