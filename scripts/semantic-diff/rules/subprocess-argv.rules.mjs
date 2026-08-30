// W0 pins: the two critical subprocess argv shapes that credential handling
// and sandbox trust depend on (see AGENTS.md's MCP section and safety
// invariant #8/#10). Both are deliberately generated in exactly ONE place
// each so a doctor/trust "recognizer" can never drift from what registration
// actually writes — these pins protect that single-source-of-truth property
// itself, not just the literal argv.
export default [
	{
		id: "argv.op-run-wrapper.exact-grammar",
		description: 'mcp.OpRunWrap is the ONE op-run wrapper grammar the launcher ever generates: `<op> run --no-masking --env-file=<refs> --`. Widening this (extra flags, different ordering, a different env-file form) changes what credentials the sbx gateway resolves at spawn, so it must stay exactly this shape. The second caller this pin used to require — workflow/doctor/mcp.go\'s pack-declared-MCP-server health probe wrapping its own exec through the SAME grammar — was deleted with the pack system in the Pix v2 cutover (docs/design/pix-v2-architecture.md §14, AC-16): there is no more pack-declared MCP classification for doctor to probe. registerServers (cmd/pix) is now the wrapper\'s only caller.',
		checks: [
			{
				file: "services/host/mcp/mcp.go",
				kind: "contains",
				region: { start: "func OpRunWrap(opPath, opRefs string, argv []string) []string {", end: "\n// RegisterServers" },
				values: ['return append([]string{opPath, "run", "--no-masking", "--env-file=" + opRefs, "--"}, argv...)'],
			},
		],
	},
	{
		id: "argv.raw-add-args.no-env-flag",
		description: 'McpRegistrar.AddArgs registers a local MCP server via `sbx mcp add <name> --command <argv[0]> --args <argv[1]> ...`. There is deliberately no `--env` form (see AGENTS.md MCP section, "no `--env`") \u2014 credentials only ever arrive via the op-run wrapper above, never as a literal env flag on the registration command.',
		checks: [
			{
				file: "services/host/mcp/mcp.go",
				kind: "contains",
				region: { start: "func (m McpRegistrar) AddArgs(name string) []string {", end: "\n// ExecArgv" },
				values: ['args := []string{"mcp", "add", name, "--command", argv[0]}', 'args = append(args, "--args", c)'],
			},
			{
				file: "services/host/mcp/mcp.go",
				kind: "notContains",
				region: { start: "func (m McpRegistrar) AddArgs(name string) []string {", end: "\n// ExecArgv" },
				values: ["--env"],
			},
		],
	},
];
