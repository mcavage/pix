// W0 pins: stdout/stderr conventions for the pix-* sandbox scope guard's
// refusal messages.
//
// The mcpStdio pin (a stdio-protocol reply loop must write only through
// os.Stdout.Write, never fmt.Println/log) is RETIRED with the code it policed:
// U11j deleted the `pix-host mcp <name>` bridge and util.go's stdio transport,
// because the bridge served zero built-in servers. pix-host frames no stdio
// protocol at all now, so there is no channel left for an unframed byte to
// corrupt. Re-pin this the day a stdio server comes back.
export default [
	{
		id: "stdio.sandbox-scope.refusal-goes-to-stderr",
		description: "A refused (non-pix-*) rm target is reported on the ERROR stream (errOut, which the launcher binds to stderr), never on out, so a caller piping stdout never mistakes a refusal for a completed removal.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ['fmt.Fprintf(errOut, "refusing %q: not a pix sandbox'],
			},
		],
	},
];
