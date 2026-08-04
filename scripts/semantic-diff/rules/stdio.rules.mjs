// W0 pins: stdout/stderr conventions for the stdio-framed subprocesses the sbx
// gateway spawns (util.go's mcpStdio) and for the pix-* sandbox scope guard's
// refusal messages. The shared risk: a single `fmt.Println`/log-to-stdout
// slipped into a stdio-protocol loop corrupts every message after it for the
// peer parsing newline-delimited JSON on stdout — a bug invisible to any test
// that does not literally frame-parse stdout, and easy for a refactor to
// introduce without noticing.
export default [
	{
		id: "stdio.mcp-stdio.protocol-channel-is-stdout-only",
		description: "mcpStdio's reply path writes ONLY through os.Stdout.Write (raw or Content-Length framed) and never through fmt.Println/log — those would inject unframed bytes into the peer's stdio channel.",
		checks: [
			{
				file: "services/host/util.go",
				kind: "contains",
				region: { start: "func mcpStdio(handle func(jsonObj) (jsonObj, bool)) {" },
				values: ["os.Stdout.Write"],
			},
			{
				file: "services/host/util.go",
				kind: "notContains",
				region: { start: "func mcpStdio(handle func(jsonObj) (jsonObj, bool)) {" },
				values: ["fmt.Println(", "log.Println(", "log.Printf("],
			},
		],
	},
	{
		id: "stdio.sandbox-scope.refusal-goes-to-stderr",
		description: "A refused (non-pix-*) rm target is reported on stderr, never stdout, so a caller piping stdout output never mistakes a refusal for a completed removal.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ['fmt.Fprintf(os.Stderr, "refusing %q: not a pix sandbox'],
			},
		],
	},
];
