// U04g: sandbox/process LIFECYCLE contracts (Story04, PRD AC-LIFE).
//
// This shard was reserved present-but-empty at W0 for exactly this task (see
// README.md's "Story04" section). It now ships two kinds of pin:
//
// 1. ACTIVE pins on contracts that are ALREADY TRUE TODAY: the U04a lease
//    foundation (services/host/lease/*.go — paths, file modes, CLOEXEC,
//    write-once instance records) and the existing `pix rm` seam
//    (services/host/workflow/launch/{sandbox,run,sbxargs}.go — non-force
//    scoping, keep polarity, pix-* scope). These are evaluated on every run
//    like any other domain shard.
//
// 2. STAGED (`activation: "story04"`) pins on contracts that describe
//    BEHAVIOR THAT DOES NOT EXIST YET — the orphan reaper, bare non-TTY `pix
//    rm` refusal, and the `-k`/`--keep` flag. Evaluating these for real today
//    would permanently redden the gate for work nobody has landed, so
//    engine.mjs's `evaluatePins` skips a pin whose `activation` key is absent
//    from scripts/semantic-diff/activation.json (or the CLI's `--activate`),
//    reporting it `pending: true` instead of failing it. Story04 turns each
//    one on by (a) landing the real behavior in the same commit that (b)
//    adds `{ key: "story04", rationale, evidence }` to activation.json —
//    exactly the same "waiver lands with the change" discipline
//    intended-changes.json already enforces for manifest waivers (see
//    checkRuleDrift in lib/engine.mjs, which also fingerprints a pin's
//    `activation` field, so silently flipping one from staged to active
//    outside that sequence is itself caught as rule drift).
//
// tests/check-semantic-diff.test.mjs's "tier 2b" activation self-tests prove
// this mechanism end-to-end: a staged pin is provably a real, currently-
// failing TODO (not a vacuous placeholder) by forcing its activation key on
// and showing it fails against THIS repo right now, then proving the SAME
// pin is silently skipped (pending, gate-green) with activation.json's
// shipped (empty) content.
export default [
	// --- ACTIVE: force is scoped to explicit user/replace seams -------------
	{
		id: "lifecycle.rm.force-scoped-to-explicit-seams",
		description:
			"`sbx rm -f` fires in exactly two seams, both explicit: RemovePixSandbox (the user-facing `pix rm <name>` command) and ApplyReplaceRm, whose RmFirst is only true when the user passed --replace. AGENTS.md safety invariant #7 (\"an existing sandbox is never force-removed... --replace is the only path that recreates one\") depends on RmFirst's condition, not merely its name.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ['env.Run("sbx", "rm", "-f", name)'],
			},
			{
				file: "services/host/workflow/launch/run.go",
				kind: "contains",
				values: ["if !plan.RmFirst {", 'env.Run("sbx", "rm", "-f", name)'],
			},
			{
				file: "services/host/workflow/launch/sbxargs.go",
				kind: "contains",
				values: ["RmFirst: replace && (state == SbxRunning || state == SbxStopped),"],
			},
		],
	},

	// --- ACTIVE: keep polarity (existing --except/--all seam) ---------------
	{
		id: "lifecycle.rm.keep-polarity-except",
		description:
			"`pix rm --all --except <name>` removes a box only when it is ABSENT from the keep set (!keepSet[b.Name]) — the direction documented in RmDescription (\"remove all but one\"). Flipping this one-word condition would silently reverse which boxes survive. The --all discovery loop is also scoped: it only ever iterates workspace.ParsePixBoxes (already pix-* filtered), so an automatic/bulk removal can never reach a non-pix sandbox in the first place.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ["for _, b := range workspace.ParsePixBoxes(raw) {", "if !keep[b.Name] {"],
			},
		],
	},

	// --- ACTIVE: stable, write-once instance identity (create/teardown) -----
	{
		id: "lifecycle.lease.instance-id-immutable",
		description:
			"lease.CreateRecord writes the immutable instance record exactly once (O_EXCL makes write-once a syscall-level guarantee, not an app-level check-then-write race) and refuses to relabel an existing lease directory under a different instance ID — a create/teardown pair can never alias two different sandbox lifetimes onto the same lease dir. ValidateInstanceID's allowlist regex is the other half: only [A-Za-z0-9._-], so an instance ID can never traverse or escape SandboxDir's path join.",
		checks: [
			{
				file: "services/host/lease/record.go",
				kind: "contains",
				values: ["if existing.InstanceID != instanceID {", "syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL"],
			},
			{
				file: "services/host/lease/paths.go",
				kind: "contains",
				values: ["^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"],
			},
		],
	},

	// --- ACTIVE: lease state paths + file/dir modes --------------------------
	{
		id: "lifecycle.lease.paths-and-modes",
		description:
			"The three lease state file names (record.json, keep.json + its keep.lock guard, lease.lock) and their permission modes (0700 sandbox dirs, 0600 files — never group/other readable or writable) are fixed literals independent production code has no reason to touch; a bulk rename or a loosened mode would otherwise sail through untouched.",
		checks: [
			{ file: "services/host/lease/record.go", kind: "contains", values: ['const recordFileName = "record.json"', "0o600"] },
			{ file: "services/host/lease/lock.go", kind: "contains", values: ['const lockFileName = "lease.lock"', "0o600"] },
			{ file: "services/host/lease/keep.go", kind: "contains", values: ['keepFileName     = "keep.json"', 'keepLockFileName = "keep.lock"', "0o600"] },
			{ file: "services/host/lease/paths.go", kind: "contains", values: ["os.Mkdir(dir, 0o700)"] },
		],
	},

	// --- ACTIVE: CLOEXEC on every lease fd -----------------------------------
	{
		id: "lifecycle.lease.cloexec",
		description:
			"Every lease-package fd is opened via openNoFollow's raw syscall.Open, which ORs in O_CLOEXEC explicitly (os.OpenFile would have set it automatically, but this bypasses that) — so a plugin subprocess launched from the daemon can never inherit a held lease lock fd across exec and deadlock a future holder. Pinned against BOTH the implementation and its own regression test so a change to either alone still trips this pin.",
		checks: [
			{ file: "services/host/lease/paths.go", kind: "contains", values: ["syscall.O_NOFOLLOW|syscall.O_CLOEXEC"] },
			{ file: "services/host/lease/lock_process_test.go", kind: "contains", values: ["func TestCLOEXEC_ChildDoesNotInheritLeaseFd("] },
		],
	},

	// --- STAGED (Story04): orphan reaper — no force, keep-absence required --
	{
		id: "lifecycle.reaper.no-force-requires-absence",
		description:
			"STAGED for Story04: the automatic/orphan sandbox reaper does not exist yet (services/host/lease/doc.go: \"no supervisor loop, no policy for WHEN to reap\"). Once built, it must (a) NEVER force-remove — only lease.TryExclusive()'s kernel-verified zero-holder proof authorizes destroying sandbox state, never a bulk `sbx rm -f` sweep — and (b) treat a KeepState as blocking: auto-remove requires its ABSENCE (ReadKeep's isSet == false), never removing while a keep is held by any identity.",
		activation: "story04",
		checks: [
			{
				file: "services/host/workflow/launch/reap.go",
				kind: "contains",
				values: ["lease.TryExclusive()", "refusing to reap: keep is held"],
			},
			{
				file: "services/host/workflow/launch/reap.go",
				kind: "notContains",
				values: ['"sbx", "rm", "-f"'],
			},
		],
	},

	// --- STAGED (Story04): bare `pix rm` refuses on a non-interactive term --
	{
		id: "lifecycle.rm.bare-nontty-refusal",
		description:
			"STAGED for Story04: `pix rm` with no name and no --all/--except (a \"bare\" invocation) run from a non-interactive (non-TTY) context does not yet refuse — RunRm currently only distinguishes \"no argv at all\" (usage + exit 2). Story04 adds an explicit refusal so a bare rm can never silently no-op (or, worse, later silently act) when nothing is there to confirm it interactively.",
		activation: "story04",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ["refusing bare `pix rm` on a non-interactive terminal"],
			},
		],
	},

	// --- STAGED (Story04): -k/--keep flag -------------------------------------
	{
		id: "lifecycle.rm.keep-short-and-long-flag",
		description:
			"STAGED for Story04: Rm's RmOptions carries only the long `--except <name>` spelling today. Story04 adds a `-k`/`--keep` flag (short and long) with the SAME keep-polarity direction pinned above in lifecycle.rm.keep-polarity-except.",
		activation: "story04",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ['case a == "-k", a == "--keep":'],
			},
		],
	},
];
