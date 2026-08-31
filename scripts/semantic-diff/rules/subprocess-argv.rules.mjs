// W0 pins: the two critical subprocess argv shapes that credential handling
// and sandbox trust depend on (see AGENTS.md's MCP section and safety
// invariant #8/#10). Both are deliberately generated in exactly ONE place
// each so a doctor/trust "recognizer" can never drift from what registration
// actually writes — these pins protect that single-source-of-truth property
// itself, not just the literal argv.
export default [
	{
		id: "argv.op-run-wrapper.exact-grammar",
		description: 'mcp.OpRunWrap is the ONE op-run wrapper grammar the launcher ever generates: `<op> run --no-masking --env-file=<refs> --`. Widening this (extra flags, different ordering, a different env-file form) changes what credentials the sbx gateway resolves at spawn, so it must stay exactly this shape. The second caller this pin used to require — workflow/doctor/mcp.go\'s pack-declared-MCP-server health probe wrapping its own exec through the SAME grammar — was deleted with the pack system in the Pix v2 cutover (docs/design/pix-v2-architecture.md §14, AC-16): there is no more pack-declared MCP classification for doctor to probe. The pack-manifest registration admin (mcp.RegisterServers, McpRegistrar, cmd/pix registerServers) was deleted in the round-4 config collapse: the environment document plus reserved built-ins are the ONE MCP declaration channel, so workflow/launch/envlaunch.go\'s opRunWrapIfAvailable is now the wrapper\'s only caller.',
		checks: [
			{
				file: "services/host/mcp/mcp.go",
				kind: "contains",
				region: { start: "func OpRunWrap(opPath, opRefs string, argv []string) []string {", end: "\n// remoteMCPRegistrationCurrent" },
				values: ['return append([]string{opPath, "run", "--no-masking", "--env-file=" + opRefs, "--"}, argv...)'],
			},
		],
	},
];
