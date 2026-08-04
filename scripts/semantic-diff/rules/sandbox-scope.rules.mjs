// W0 pins: the "pix-*" sandbox naming scope (AGENTS.md safety invariant #12:
// "`pix rm` is scoped to `pix-*` sandboxes only. It can never reach a sandbox
// it did not create."). Three independent files each hardcode the same
// "pix-" literal; a bulk rename that touched all three consistently (e.g. to
// widen or shrink the prefix) would otherwise sail through untouched.
export default [
	{
		id: "sandbox-scope.prefix.rm-refuses-foreign",
		description: "workflow/launch's rm path refuses to remove any name that is not pix-* prefixed, and says so on stderr rather than silently skipping.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ['strings.HasPrefix(n, "pix-")', 'refusing %q: not a pix sandbox'],
			},
		],
	},
	{
		id: "sandbox-scope.prefix.ls-filter",
		description: "workspace.ParsePixBoxes only ever surfaces pix-* rows from `sbx ls` output.",
		checks: [
			{
				file: "services/host/workspace/sandboxls.go",
				kind: "contains",
				values: ['strings.HasPrefix(name, "pix-")'],
			},
		],
	},
	{
		id: "sandbox-scope.prefix.derive-name",
		description: "workspace.DeriveSandboxName still derives every default sandbox name as \"pix-\" + the workspace directory's base name.",
		checks: [
			{
				file: "services/host/workspace/resolve.go",
				kind: "contains",
				region: { start: "func DeriveSandboxName(ws string) string {", end: "\n}\n" },
				values: ['return "pix-" + base'],
			},
		],
	},
];
