// W0 pins: the two critical subprocess argv shapes that credential handling
// and sandbox trust depend on (see AGENTS.md's MCP section and safety
// invariant #8/#10). Both are deliberately generated in exactly ONE place
// each so a doctor/trust "recognizer" can never drift from what registration
// actually writes — these pins protect that single-source-of-truth property
// itself, not just the literal argv.
export default [
	{
		id: "argv.op-run-wrapper.exact-grammar",
		description: 'Pin the single op-run credential wrapper. Preview and launch both use workflow/env.EnvironmentFacts; the end anchor follows the next surviving function after deletion of unused MCP administration code.',
		checks: [
			{
				file: "services/host/mcp/mcp.go",
				kind: "contains",
				region: { start: "func OpRunWrap(opPath, opRefs string, keys, argv []string) []string {", end: "\n// outputContainsCanonicalEndpoint" },
				values: ['return append([]string{"/bin/bash", "-c", scopedOpRun, "pix-mcp", opPath, opRefs, strings.Join(keys, ",")}, argv...)'],
			},
		],
	},
];
