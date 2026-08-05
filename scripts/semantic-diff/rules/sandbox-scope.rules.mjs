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
		// U04e retired workspace.DeriveSandboxName (bare "pix-<basename>"): it had
		// already drifted from run's default, which is sandbox.Name's digest form.
		// The pix-* SCOPE the reaper and `pix rm` depend on is what actually
		// matters, and it now has exactly one producer.
		id: "sandbox-scope.prefix.derive-name",
		description: "sandbox.Name is the ONE default-sandbox-name derivation, and every name it produces is inside the pix-* scope `pix rm`/the reaper refuse outside of.",
		checks: [
			{
				file: "services/host/sandbox/name.go",
				kind: "contains",
				values: ['const Prefix = "pix-"'],
			},
		],
	},
];
