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
//    (U04d NOTE: the three formerly-staged pins below are now ACTIVE. Story04's
//    teardown/reaper landed in the same commit that added `story04` to
//    scripts/semantic-diff/activation.json and the matching intended-change
//    entries for un-staging them — the exact "the waiver lands with the change"
//    sequence this header describes.)
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
	// --- ACTIVE: force is scoped to the ONE explicit user seam --------------
	{
		id: "lifecycle.rm.force-scoped-to-explicit-seams",
		description:
			"`sbx rm -f` fires in exactly ONE seam, and it is explicit: RemovePixSandbox, reachable only from a named `pix rm --force`. U04e deleted the second one (ApplyReplaceRm, gated on --replace): it removed with no zero-holder proof and outside the lifecycle lock, so it could destroy a sandbox another shell was live in. AGENTS.md safety invariant #7 now depends on there being no other caller. sbx v0.38 (docs/upstream/sbx-0.38-noninteractive-rm.md) made `-f` a confirmation bypass every non-interactive removal needs, so RemovePixSandbox now composes it through the SAME shared planner (sandbox.PlanForceRemove) every other forced-argv caller uses — still the ONE seam, just no longer a literal argv this file hand-rolls.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ["argv, err := sandbox.PlanForceRemove(name)", 'env.Run("sbx", argv...)'],
			},
			{
				// The retired flag must stay retired: a table entry is what makes
				// `pix run --replace` answer with the recovery path instead of
				// quietly force-removing again.
				file: "services/host/cmd/pix/retired.go",
				kind: "contains",
				values: ['retiredKey("run", "--replace"):'],
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
			"The lease state file names (record.json, keep.json + its keep.lock guard, refs.lock, lifecycle.lock) and their permission modes (0700 sandbox dirs, 0600 files — never group/other readable or writable) are fixed literals independent production code has no reason to touch; a bulk rename or a loosened mode would otherwise sail through untouched. U04c1 resharded the single lease.lock into refs.lock (RefLease) + lifecycle.lock (LifecycleLock) — this pin now tracks both.",
		checks: [
			{ file: "services/host/lease/record.go", kind: "contains", values: ['const recordFileName = "record.json"', "0o600"] },
			{ file: "services/host/lease/lock.go", kind: "contains", values: ['const refsLockFileName = "refs.lock"', "0o600"] },
			{ file: "services/host/lease/lifecycle.go", kind: "contains", values: ['const lifecycleLockFileName = "lifecycle.lock"'] },
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

	// --- ACTIVE (U04c2): the create/attach ordering, lock by lock ------------
	{
		id: "lifecycle.session.record-before-lifecycle-unlock",
		description:
			"U04c2: launch.RunSession performs the whole transition inside lease.AttachRefUnderLifecycle's fn — lifecycle EXCLUSIVE held — so the child is started and every create-time fact (immutable instance id, fingerprint, exact pi invocation) is recorded BEFORE the lifecycle lock is released and BEFORE the session is waited for. A creator SIGKILLed mid-session therefore still leaves a complete record, and a reaper acquiring the lifecycle lock next can never see a recorded sandbox with zero holders. The refs SHARED reference is taken by the SAME helper while lifecycle is still held; child.Wait() is called only after it returns. U04f closed the follow-on expired-context gap: fn can legitimately run for the whole ~15-minute create-poll window while still holding the lifecycle lock, so the refs SHARED acquire AFTER fn returns takes its OWN fresh, RefAcquireTimeout-bounded context (detached from ctx via context.WithoutCancel) instead of reusing the caller's ctx, which by then is already past whatever deadline it was sized for.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: [
					"ref, err := lease.AttachRefUnderLifecycle(ctx, dir, func() error {",
					"child, terr = startSessionTransition(spec, deps)",
					"return child.Wait()",
				],
			},
			{
				file: "services/host/lease/ordering.go",
				kind: "contains",
				values: [
					"func AttachRefUnderLifecycle(ctx context.Context, dir string, fn func() error) (*RefLease, error) {",
					"if err := lc.AcquireExclusive(ctx); err != nil {",
					"refCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RefAcquireTimeout)",
					"if err := rl.AcquireShared(refCtx); err != nil {",
				],
			},
			{
				file: "services/host/workflow/launch/run.go",
				kind: "contains",
				values: ["func StartSbxSession(cmd *exec.Cmd, poll CreatePoll, creating bool, sandbox string) (*SessionChild, error) {"],
			},
		],
	},

	{
		id: "lifecycle.session.reprobe-under-the-lifecycle-lock",
		description:
			"U04c2: the create-vs-attach decision made before the lock is ADVISORY; RunSession re-probes the runtime under the lifecycle lock and REFUSES (SessionRefused, nothing started) when the answer changed — a create whose sandbox now exists, or an attach whose sandbox vanished. It never force-removes anything to resolve the race, so AGENTS.md safety invariant #7 holds by construction.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: [
					"if state := ProbeTaskSandbox(deps.Env, spec.Name); sandboxAppeared(state) {",
					"if state := ProbeTaskSandbox(deps.Env, spec.Name); state == SbxAbsent {",
					"nothing was created or removed",
				],
			},
			{
				file: "services/host/workflow/launch/session.go",
				kind: "notContains",
				values: ['"rm", "-f"'],
			},
		],
	},

	{
		id: "lifecycle.session.create-record-requires-verified-instance-id",
		description:
			"U04c2: RecordSessionCreation writes the immutable lease record ONLY when a fresh `sbx ls --json` positively names a schema-verified instance id for the sandbox — an absent listing, a parse failure, an unverified row, or a present-but-empty instance id all record NOTHING, leaving the session permanently UNOWNED rather than inventing an identity that would authorize a future teardown. A missing or CORRUPT record attaches unowned with the safe recomputed invocation (readSessionState's found=false covers both identically), and -k/--keep refuses to bind a keep to a session with no recorded identity.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: [
					"if found == nil || !found.IdentityVerified || found.InstanceID == nil || *found.InstanceID == \"\" {",
					"return false, nil // present but unowned: no verifiable instance id to record",
					"recorded := SessionRecorded(spec.Key)",
				],
			},
		],
	},

	{
		id: "lifecycle.session.digest-name-keys-lease-state",
		description:
			"U04c2: a session's lease/keep/fingerprint state is keyed by sandbox.Name(workspace) (SessionName) — the deterministic, digest-suffixed identity U04b's sandbox package derives — and `pix run`'s DEFAULT sandbox name is that same digest form, not workspace.DeriveSandboxName's bare \"pix-<basename>\". Two workspaces sharing a basename can never alias the same lease directory or the same box. An explicit --name still travels verbatim.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: ["func SessionName(workspace string) string { return sandbox.Name(workspace) }"],
			},
			{
				file: "services/host/cmd/pix/run_cmd.go",
				kind: "contains",
				values: ["return sandbox.Name(workspace)"],
			},
		],
	},

	{
		id: "lifecycle.session.fingerprint-mismatch-refuses-attach",
		description:
			"U04c2: an attach whose RECORDED create-time fingerprint diverges from the current one refuses outright with the diverged keys named, under the lifecycle lock, rather than silently attaching to a sandbox created under different terms. A sandbox with no recorded fingerprint (found=false) has nothing to compare against and attaches unowned instead of refusing.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: [
					"if diverged, found := CheckSessionFingerprint(spec.Key, spec.Fingerprint); found && len(diverged) > 0 {",
					"refusing to attach. %s",
					"RecreateGuidance(spec.Name)",
				],
			},
		],
	},

	{
		id: "lifecycle.session.attach-execs-stored-invocation",
		description:
			"U04c2: an attach to a POSITIVELY IDENTIFIED, RUNNING sandbox re-invokes pi through `sbx exec -it` (interactive) / `-i` (piped) with the STORED create-time invocation replayed verbatim — never asking sbx to re-derive a command from the container's spec. A stopped or schema-unverified row keeps the legacy `sbx run --name` reattach argv, because exec has no start of its own.",
		checks: [
			{
				file: "services/host/workflow/launch/sbxargs.go",
				kind: "contains",
				values: ["Command: append([]string{\"pi\"}, invocation...),"],
			},
			{
				file: "services/host/sandbox/argv.go",
				kind: "contains",
				values: ['args := []string{"exec", ttyFlag(o.TTY), o.Name}', 'return "-it"'],
			},
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: ["execArgs, aerr := BuildAttachArgv(spec.Name, spec.AttachTTY, invocation)"],
			},
		],
	},

	{
		id: "lifecycle.run.bare-nontty-refusal-no-create",
		description:
			"U04c2: a BARE positional launch (never the explicit `run` verb) on a non-interactive terminal refuses before the root parser ever runs — no create, no attach, stderr-only guidance naming the resolved path, exit 2. An explicit `pix run DIR` from the same non-interactive shell is unaffected and gets `sbx exec -i`.",
		checks: [
			{
				file: "services/host/cmd/pix/root.go",
				kind: "contains",
				values: ["if !d.Interactive {", "fmt.Fprintf(d.Err, bareNonTTYRefusalFmt, resolvedBareArgPath(a))"],
			},
		],
	},

	// --- ACTIVE (U04d): reaper — no force, keep-absence required ------------
	{
		id: "lifecycle.reaper.no-force-requires-absence",
		description:
			"U04d (Story04) landed the automatic last-shell/orphan reaper in services/host/workflow/launch/reap.go, and it must stay this shape: (a) it NEVER force-removes — only lease.TryExclusive()'s kernel-verified zero-holder proof (reached via lease.TryReapProof) authorizes destroying sandbox state, never a bulk `sbx rm -f` sweep — and (b) a KeepState BLOCKS the automatic path: auto-remove requires its ABSENCE, never removing while a keep is held by any identity.",
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

	// --- ACTIVE (U04d): bare `pix rm` refuses on a non-interactive terminal -
	{
		id: "lifecycle.rm.bare-nontty-refusal",
		description:
			"U04d (Story04): `pix rm` with no name and no --all/--orphans (a \"bare\" invocation) run from a non-interactive (non-TTY) context REFUSES explicitly, in the domain (launch.validateRmShape) rather than the command layer, so a bare rm can never silently no-op — or, worse, silently act — when nothing is there to confirm it interactively.",
		checks: [
			{
				file: "services/host/workflow/launch/sandbox.go",
				kind: "contains",
				values: ["refusing bare `pix rm` on a non-interactive terminal"],
			},
		],
	},

	// --- ACTIVE (U04d): -k/--keep flag ---------------------------------------
	{
		id: "lifecycle.rm.keep-short-and-long-flag",
		description:
			"U04d (Story04): `pix rm` carries the `-k`/`--keep` spelling (short and long) for its keep set, with the SAME keep-polarity direction pinned above in lifecycle.rm.keep-polarity-except. The flag is DECLARED where every other pix flag is declared — the one kong-parsed command struct in cmd/pix — and both spellings fold into RmOptions.Except, so the polarity pin keeps covering the direction for either one. It is pinned at the declaration AND at the fold, because dropping either half silently loses the short flag or silently drops --keep's names.",
		checks: [
			{
				file: "services/host/cmd/pix/root.go",
				kind: "contains",
				values: [
					'short:"k" help:"With --all: keep this one (repeatable)."',
					"append(append([]string(nil), c.Keep...), c.Except...),",
				],
			},
		],
	},

	// --- ACTIVE (U04d): teardown ordering, proof before removal -------------
	{
		id: "lifecycle.teardown.ref-closed-before-the-proof",
		description:
			"U04d: the last shell out CLOSES its own refs SHARED reference BEFORE attempting the reaper's proof, and only then calls TeardownSandbox. The order is the mechanism, not a style choice: TryReapProof's refs EXCLUSIVE can never succeed while this process still holds its own SHARED reference, so a teardown attempted before the Close would report \"busy\" against itself on every exit and the sandbox would never be reaped at all.",
		checks: [
			{
				file: "services/host/workflow/launch/session.go",
				kind: "contains",
				values: [
					"werr := child.Wait()",
					"if cerr := ref.Close(); cerr != nil {",
					"reportTeardown(deps.Warn, TeardownSandbox(deps.Env, spec.Key, spec.Name, TriggerSession, deps.Teardown))",
				],
			},
		],
	},

	{
		id: "lifecycle.teardown.absent-probe-before-state-clear",
		description:
			"U04d: the recorded identity is cleared ONLY after the removal's absence was POSITIVELY confirmed by a bounded re-probe (clearedResult is reached from the SbxAbsent branch and from the already-absent branch, never from the rm's own exit status). A removal whose outcome could not be confirmed is TeardownFailed and RETAINS its state, because an unknown outcome must keep its evidence. The bounds are pinned too: 15s for the mutating rm, 3s per absent probe with 2 retries, and a 20s total ceiling every step is clamped against (budgetedTimeout), so the composed worst case cannot exceed the ceiling.",
		checks: [
			{
				file: "services/host/workflow/launch/reap.go",
				kind: "contains",
				values: [
					"TeardownRmTimeout    = 15 * time.Second",
					"TeardownProbeTimeout = 3 * time.Second",
					"TeardownProbeRetries = 2",
					"TeardownBudget       = 20 * time.Second",
					"if probeStateWithin(env, name, within) == SbxAbsent {",
					"its state is retained",
				],
			},
		],
	},

	{
		id: "lifecycle.teardown.journal-bounded-0600",
		description:
			"U04d: every teardown attempt — including every refusal — is journalled to a BOUNDED, 0600 JSONL file in the STATE dir (AGENTS.md safety invariant #4), capped in both directions a log can grow (entries and bytes). U04f hardened the write itself two ways: SERIALIZED across processes (withTeardownJournalGuard's flock on a dedicated lock file, because an orphan sweep and a session's own teardown are frequently two different pix-host invocations doing a read-modify-write on the SAME journal path, which a unique temp name alone does not stop from silently dropping one writer's entry) and written through a UNIQUE same-dir temp file + rename (writeFileAtomic's os.CreateTemp, not a fixed '.tmp' name, so two racing writers can never interleave into one file or have one commit the other's half-written temp) so the mode is exact and a symlink at the path is replaced rather than written through.",
		checks: [
			{
				file: "services/host/workflow/launch/reap.go",
				kind: "contains",
				values: [
					'TeardownJournalName       = "teardown.jsonl"',
					"TeardownJournalMaxEntries = 200",
					"teardownJournalMaxBytes   = 64 * 1024",
					"return withTeardownJournalGuard(path, func() error {",
					"syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)",
					"return writeFileAtomic(path, []byte(strings.Join(lines, \"\\n\")+\"\\n\"), 0o600)",
				],
			},
			{
				file: "services/host/workflow/launch/atomic.go",
				kind: "contains",
				values: [
					"func writeFileAtomic(path string, data []byte, perm os.FileMode) error {",
					'tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")',
					"return os.Rename(tmpPath, path)",
				],
			},
		],
	},
];
