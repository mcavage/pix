# Homebrew distribution - build plan

Status: PLAN. Not implemented. Written in a Linux sandbox with no macOS, no
Homebrew, no Gatekeeper access. Every macOS-specific claim below is reasoning
from source, not observation - the prototype steps exist to kill that risk
before the real stories are built on top of it.

Decisions below are CLOSED. Do not relitigate them.

- macOS only. Linux build stays in CI (it's free) but the formula, docs, and
  new tests are darwin-only.
- New repo `mcavage/homebrew-tap` → `brew install mcavage/tap/pix`.
- A FORMULA installing prebuilt binaries from the GitHub release. Not
  build-from-source, not a cask.
- ONE formula owns BOTH binaries (`pix` + `pix-host`), from ONE tarball per
  arch.
- The formula declares zero dependencies. `pix setup` owns prereqs (`op`,
  `sbx`, `ollama`, `gh`).
- `pix upgrade` stays, shrinks to a provenance router.
- Collapse: kit + image pin to the launcher's stamped version. Delete the
  runtime latest-stable lookup and its cache. Keep `--kit-ref` and
  `version_pin`.
- No goreleaser. Releases stay auto-published on every push to main, no
  approval gate.

## Prototype order (do this FIRST, on a Mac)

Each of these kills one specific unknown before any story below is built for
real. If a prototype step disproves an assumption, stop and fix the plan
before continuing - do not build stories 1-10 on an unverified premise.

**P1. Hardcoded-version tap, both arches. NEEDS A MAC (arm + intel).**
Create `mcavage/homebrew-tap` with a formula that hardcodes today's release
tag and sha256 (skip Step 8's automation entirely for now). `brew install
mcavage/tap/pix` on an arm64 Mac and an intel Mac (or Rosetta shell on arm,
if that's all you have - but a real intel machine is better since Rosetta
changes the trust story). Confirms three things in one shot:
- An unsigned Go binary installed via a formula runs with **no Gatekeeper
  prompt**. Reasoning: brew fetches with `curl`, which does not set the
  `com.apple.quarantine` xattr, and Go ad-hoc-signs arm64 binaries at link
  time. If this is wrong, the whole formula plan needs a notarization step
  (out of scope, would kill "no goreleaser").
- `on_arm` / `on_intel` URL selection actually resolves to the right asset.
- `man1.install` actually installs the manpage (`services/host/cmd/pix/pix.1`
  exists already - confirm `man pix` finds it after install).

**P2. `findHostBinary()` under a real keg. NEEDS A MAC.**
Install via the tap, then run `pix serve` (or any verb that calls
`findHostBinary()`, `main.go:272`) and confirm the sibling lookup in
`main.go:281-283` lands on the Cellar's `pix-host`, not PATH. The comment in
`upgrade.go` around `sameFile` notes `os.Executable()` semantics differ by
platform - on darwin `os.Executable()` does **not** resolve symlinks the way
Linux's `/proc/self/exe` does. Homebrew installs the actual binaries into
`Cellar/pix/<version>/bin/` and symlinks them from `$(brew --prefix)/bin/`.
Verify by hand what `os.Executable()` returns when `pix` is invoked as
`$(brew --prefix)/bin/pix` (a symlink) - does it return the symlink path
(`/opt/homebrew/bin/pix`) or the resolved Cellar path? The sibling-lookup in
`findHostBinary()` only works if `filepath.Dir(self)` contains `pix-host`
too - a bare `$(brew --prefix)/bin` symlink dir does, since brew symlinks
both binaries there, so this SHOULD already work without a code change. Do
not ship the formula on faith - write down what `os.Executable()` actually
returned, in the build issue.

**P3. Combined tarball vs install.sh. Does NOT need a Mac** (install.sh runs
under `sh` on Linux too, and the tarball itself is arch-labelled, not
platform-verified by the script).
Build a `pix_<version>_darwin_arm64.tar.gz` with both binaries per Step 6,
then confirm the EXISTING loose-asset install.sh flow is untouched: run
`install.sh` against a temp `PIX_PREFIX` and confirm it still finds
`pix-darwin-arm64` / `pix-host-darwin-arm64` (the loose assets), because the
combined tarball is additive, not a replacement. This is a regression check
on Step 6, cheap to do before touching the tap.

**P4. The shadowing scenario end to end. NEEDS A MAC.**
curl-install an older `pix` to `~/.local/bin` (`install.sh`'s default
prefix), then `brew install mcavage/tap/pix` (the newer version), and look at
what `command -v pix` resolves to, what `pix` (bare, prints status) shows,
and what `pix version` prints, given your actual shell's PATH ordering.
`~/.local/bin` is very commonly ahead of `/opt/homebrew/bin` on a
`.zshrc`/`.bashrc` that predates Homebrew, so the running-brew-but-shadowed
case is the default outcome for anyone who curl-installed before this ships,
not an edge case. Do this **before** writing Step 4's duplicate-install
detector - the detector's exact wording and the doctor check it feeds should
be written against the actually-observed strings (`command -v pix`,
`which -a pix`), not a guess. Do this before announcing brew as the primary
path in docs (Step 10) - a doctor check with unverified copy is worse than
none.

**P5. `serve install` then `brew uninstall`, then watch launchd. NEEDS A
MAC.**
`pix serve install` registers a launchd agent that runs `pix-host serve`.
Formulae have no uninstall hook (this is a Homebrew fact, not something to
verify, but the CONSEQUENCE needs observing): `brew uninstall pix` deletes
the Cellar binaries out from under a launchd job that has no idea the
formula existed. Run `pix serve install`, confirm it's up
(`pix serve status`), then `brew uninstall mcavage/tap/pix` directly (skip
`pix state uninstall`), and watch `launchctl list | grep pix` and
`~/Library/Logs` (or wherever the launchd stderr redirects) for the
respawn-fail loop. This determines exactly how loud the formula's `caveats`
block and the `pix state uninstall` step need to be in Step 7/Step 4 - if
launchd's `KeepAlive` just quietly retries a dead binary forever with no
visible symptom, the caveats need to say "you MUST run `pix state uninstall`
first" in the strongest language a caveats block supports; if it's obviously
broken (crash loop in Console.app), a lighter caveats note may do.

Prototype exit criteria: P1-P3 pass as reasoned (or the plan below is
revised to match what's actually true), P4's exact output strings are copied
into Step 4's acceptance criteria, and P5's observed behavior is copied into
Step 7's caveats text. Do not proceed to Step 1 with an assumption from this
section still unverified.

### Prototype observations (2026-07-27)

- P1 passed on Apple Silicon: Homebrew selected the arm64 archive, neither
  binary had `com.apple.quarantine`, Go's linker-provided ad-hoc signature was
  present, both binaries ran without a Gatekeeper prompt, and the Homebrew man
  page rendered. Intel remains for tap CI or a physical Intel Mac.
- P2 passed. Darwin's `os.Executable()` returned the invoked symlink path and
  Homebrew linked both binaries into the same prefix `bin` directory, so the
  sibling lookup found `pix-host`.
- P3 passed. The existing installer downloaded and verified the loose
  `pix-darwin-arm64` and `pix-host-darwin-arm64` assets after combined archives
  were added to the same release.
- P4 reproduced the shadow: bare `pix` resolved to `~/.local/bin/pix` at
  version 0.1.7 while `/opt/homebrew/bin/pix` reported 0.1.8. Homebrew itself
  warned that both `pix` and `pix-host` were shadowed and named the earlier
  paths.
- P5 corrected one assumption. Removing the keg does not kill an already
  running `pix-host`; launchd continued to report that process as running and
  no immediate error reached `serve.log`. The failure is latent: the next
  launch after logout, crash, or service restart cannot execute the missing
  Cellar path. Caveats must require `pix state uninstall` first, but must not
  claim that an immediate visible respawn loop was observed.

---

## Step 1 - provenance detector

**Needs a Mac:** to verify the Cellar-path detection against a real keg (P2
covers most of this); the function itself can be written and unit-tested
without one, since the darwin-specific paths are string literals it can be
handed via a fake `os.Executable`-equivalent seam.

**Goal.** One function, `installChannel()`, that answers "how did this
binary get here" without ever shelling out to `brew` on the happy path,
because this runs inside `status`/`doctor`, which are interactive.

**Files touched.**
- New: `services/host/cmd/pix/provenance.go`
- Consumers: `upgrade.go` (Step 2), `doctor.go`, `status.go`, `reset.go`
  (Step 4)

**Implementation sketch.**

```go
package main

// provenance.go - where did this binary come from? Consumed by upgrade,
// doctor, status, and uninstall so all four agree on one answer instead of
// four ad-hoc guesses.

import (
	"os"
	"path/filepath"
	"strings"
)

type installChannel int

const (
	channelUnknown installChannel = iota // a real answer - never guessed from a path prefix
	channelHomebrew
	channelInstaller // install.sh / curl, or `pix upgrade`'s own re-run of it
	channelLocalDev  // `make install`, unreleased version
)

func (c installChannel) String() string {
	switch c {
	case channelHomebrew:
		return "Homebrew"
	case channelInstaller:
		return "Installer"
	case channelLocalDev:
		return "LocalDev"
	default:
		return "?"
	}
}

// provenance is installChannel plus the evidence that produced it, so a
// doctor/status line can show its work instead of asserting a verdict.
type provenance struct {
	Channel  installChannel
	Resolved string // the realpath os.Executable() resolved to, for display
	Evidence string // one line explaining the verdict
}

// detectInstallChannel is the seam tests drive: hand it the resolver +
// symlink-eval + getenv functions instead of calling os.Executable directly.
func detectInstallChannel(
	executable func() (string, error),
	evalSymlinks func(string) (string, error),
	getenv func(string) string,
) provenance {
	self, err := executable()
	if err != nil {
		return provenance{Channel: channelUnknown, Evidence: "os.Executable failed: " + err.Error()}
	}
	resolved, err := evalSymlinks(self)
	if err != nil {
		// A broken symlink or permission error: report what we have, don't guess.
		return provenance{Channel: channelUnknown, Resolved: self, Evidence: "EvalSymlinks failed: " + err.Error()}
	}

	if !isReleased(version) {
		return provenance{Channel: channelLocalDev, Resolved: resolved, Evidence: "unreleased build (" + version + ")"}
	}

	// Homebrew's Cellar layout is /<prefix>/Cellar/pix/<ver>/bin/pix. Match the
	// path COMPONENT, not a substring, so a user directory literally named
	// "Cellar" some day can't false-positive.
	if hasPathComponent(resolved, "Cellar") && hasPathComponent(resolved, "pix") {
		ev := "resolved path contains /Cellar/pix/"
		if hp := getenv("HOMEBREW_PREFIX"); hp != "" {
			if strings.HasPrefix(resolved, hp) {
				ev += ", under $HOMEBREW_PREFIX"
			} else {
				// The env var disagrees with the path - still Homebrew (nested taps,
				// multiple installed prefixes), but say so; this is evidence, not a
				// silent override.
				ev += ", but outside $HOMEBREW_PREFIX (" + hp + ") - unusual, still treating as Homebrew"
			}
		}
		return provenance{Channel: channelHomebrew, Resolved: resolved, Evidence: ev}
	}

	return provenance{Channel: channelInstaller, Resolved: resolved, Evidence: "resolved path: " + resolved}
}

func hasPathComponent(path, want string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == want {
			return true
		}
	}
	return false
}

// installChannelNow is the production entry point: real os.Executable,
// filepath.EvalSymlinks, os.Getenv.
func installChannelNow() provenance {
	return detectInstallChannel(os.Executable, filepath.EvalSymlinks, os.Getenv)
}
```

Note what this deliberately does NOT touch: `installPrefix()` in
`upgrade.go:104-120` stays exactly as-is. Its whole reason to exist is
spelling the path the way `command -v pix` spells it (unresolved), because
`install.sh`'s collision preflight gates on `command -v`. Symlink resolution
is a DIFFERENT question ("where does this binary really live") asked for a
different reason (Homebrew detection), so it belongs only in the new
detector, never folded into `installPrefix()`.

**Acceptance criteria.**
- `go test ./services/host/cmd/pix/ -run TestDetectInstallChannel -v` - new
  table-driven test feeding fake resolvers:
  - self=`/opt/homebrew/bin/pix`, resolved=`/opt/homebrew/Cellar/pix/0.3.0/bin/pix`,
    `HOMEBREW_PREFIX=/opt/homebrew` → `channelHomebrew`.
  - self=`/home/x/.local/bin/pix`, resolved=same (no symlink) → `channelInstaller`.
  - unreleased `version` (`dev`, or a git-sha-suffixed local build) →
    `channelLocalDev` regardless of path.
  - `os.Executable` erroring → `channelUnknown`, `.String() == "?"`.
- Manual, needs Mac: after a real `brew install mcavage/tap/pix`, add a
  temporary `pix debug-provenance` verb (delete before merge, or gate behind
  an unexported test-only build) that prints `installChannelNow()` and
  confirm it prints `Homebrew` with the real Cellar path in `Evidence`.

---

## Step 2 - `pix upgrade` becomes a router

**Needs a Mac:** only to observe the real `brew upgrade` prompt/output
shape; the routing logic and non-Homebrew paths are testable without one.

**Goal.** Homebrew channel: print `brew upgrade mcavage/tap/pix`, offer
`[y/N]`, run it on consent, RE-PROBE (call `installChannelNow()` /
version-check again), only print a success word after that probe confirms
the new version is actually running. Non-TTY: print the command, exit 0, no
prompt. `--force` must refuse (never overwrite a keg). `--version` on
Homebrew refuses and explains pinning is a brew operation. `--check` behaves
identically across every channel (doctor/status route through it).

**Files touched.** `services/host/cmd/pix/upgrade.go` (rewrite
`runUpgrade`), new small helper split for the routing, `upgrade_test.go`
(existing, extend).

**Implementation sketch.**

```go
// runUpgrade is the `pix upgrade` entry point. It routes on install
// provenance FIRST; only the Installer/LocalDev branches reach the existing
// install.sh-driven flow.
func runUpgrade(argv []string) {
	for _, a := range argv {
		if a == "-h" || a == "--help" {
			fmt.Print(upgradeUsage)
			return
		}
	}
	o, err := parseUpgradeArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix upgrade: %v\n\n%s", err, upgradeUsage)
		os.Exit(2)
	}

	prov := installChannelNow()
	switch prov.Channel {
	case channelHomebrew:
		runUpgradeHomebrew(o, prov, os.Stdin, os.Stdout, os.Stderr)
		return
	case channelLocalDev:
		if !o.Force {
			fmt.Fprintf(os.Stderr, "pix upgrade: %v\n",
				fmt.Errorf("this is a local build (%s), not a release - `pix upgrade` would replace it with a published one.\nRebuild from your checkout with `make install`, or force it: pix upgrade --force", version))
			os.Exit(1)
		}
		// falls through to the existing installer-driven path below
	}
	// ... existing Installer-channel body (today's runUpgrade), unchanged ...
}

// runUpgradeHomebrew is the Homebrew branch: never touches the keg directly,
// always defers to `brew upgrade`, and never prints a success word without a
// post-mutation probe.
func runUpgradeHomebrew(o upgradeOpts, prov provenance, stdin io.Reader, stdout, stderr io.Writer) {
	const cmdline = "brew upgrade mcavage/tap/pix"

	if o.Version != "" {
		fmt.Fprintln(stderr, "pix upgrade: this is a Homebrew install; pinning a version is a Homebrew operation, not pix's.")
		fmt.Fprintf(stderr, "  pin:      brew pin mcavage/tap/pix   (after installing the version you want)\n")
		fmt.Fprintf(stderr, "  install a specific tag: brew install mcavage/tap/pix@<version>  (if the tap carries versioned formulae)\n")
		os.Exit(1)
	}
	if o.Force {
		fmt.Fprintln(stderr, "pix upgrade: --force has no effect on a Homebrew install; a brew keg is not a thing pix overwrites.")
		fmt.Fprintf(stderr, "  run it yourself if you mean it: %s\n", cmdline)
		os.Exit(1)
	}
	if o.Check {
		fmt.Printf("pix %s installed via Homebrew.\n  check:  brew outdated mcavage/tap/pix\n  upgrade: %s\n", version, cmdline)
		return
	}

	fmt.Printf("pix %s is installed via Homebrew.\n  %s\n", version, cmdline)
	if !isTTY(stdout) {
		// Non-interactive: print and stop. Never assume consent, never prompt.
		return
	}
	fmt.Print("Run it now? [y/N] ")
	var answer string
	fmt.Fscanln(stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		return
	}

	cmd := exec.Command("brew", "upgrade", "mcavage/tap/pix")
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "pix upgrade: brew upgrade failed: %v\n", err)
		os.Exit(1)
	}

	// Re-probe. Success words are earned by a probe (house rule) - never print
	// "upgraded" just because the command exited 0; confirm the new binary
	// actually reports a different (newer) version.
	newVersion, verr := probeInstalledVersion()
	if verr != nil {
		fmt.Fprintf(stderr, "pix upgrade: brew upgrade exited 0 but could not verify the result: %v\n", verr)
		os.Exit(1)
	}
	fmt.Printf("verified: pix %s\n", newVersion)
}
```

`isTTY` and `probeInstalledVersion` are small new helpers (`probeInstalledVersion`
re-execs `pix version` - or reads `version` directly if this process is about
to exit anyway and nothing else needs re-exec; simplest correct option is
shelling to `command -v pix` then running `<that> version` so it reflects
PATH, not this process's own stale in-memory `version`).

**Acceptance criteria.**
- `go test ./services/host/cmd/pix/ -run TestUpgrade -v` extended with:
  `TestRunUpgradeHomebrewNonTTYPrintsAndExits`, `TestRunUpgradeHomebrewForceRefuses`,
  `TestRunUpgradeHomebrewVersionFlagRefuses`, `TestRunUpgradeHomebrewCheckMatchesOtherChannels`.
- Manual, needs Mac: `brew install mcavage/tap/pix` (an old version via P1's
  hardcoded tap, or after Step 8 ships), then `pix upgrade` at a real
  terminal → prompt shown, `y` → runs `brew upgrade`, ends with a line
  starting `verified: pix `. Then `pix upgrade < /dev/null` (non-TTY) →
  prints the command, exit code `0`, no prompt, no brew invocation
  (`echo $?` after).

---

## Step 3 - `install.sh` refuses a Homebrew prefix

**Needs a Mac:** to confirm the `/opt/homebrew` and `/usr/local/Homebrew`
literals are still correct and that `brew --prefix` succeeds when brew is on
PATH; the shell logic itself runs fine on Linux for review, since it's `sh`.

**Goal.** The same corruption Step 2 fixes for `pix upgrade` is reachable by
a hand-typed `PIX_PREFIX=$(brew --prefix)/bin`. Refuse before any download.

**Files touched.** `install.sh`.

**Implementation sketch.** Insert right after `PREFIX=` is computed, before
`main()` dispatches to `do_install`/`do_uninstall` - must run before
`preflight_collision` and before `fetch`:

```sh
# --- Homebrew-prefix guard --------------------------------------------------
# Installing pix's real binaries into a Homebrew-managed prefix would overwrite
# brew's own symlinks with plain files; the next `brew upgrade`/`brew uninstall`
# then silently no-ops or fights this script. Same corruption class `pix
# upgrade` fixes for the running-binary case (see upgrade.go); this is the
# hand-typed-PIX_PREFIX path into the same trap.
guard_homebrew_prefix() {
	brew_prefix=""
	if have brew; then
		brew_prefix="$(brew --prefix 2>/dev/null || true)"
	fi
	case "$PREFIX" in
		"${brew_prefix:-__no_brew__}"* ) ;;
		/opt/homebrew* | /usr/local/Homebrew* ) brew_prefix="$PREFIX" ;;
		*) return 0 ;;
	esac
	err "PIX_PREFIX ($PREFIX) is a Homebrew-managed prefix."
	err "Installing pix's real binaries there will conflict with Homebrew's own"
	err "symlinks the next time you 'brew upgrade' or 'brew uninstall' anything."
	err ""
	err "Use Homebrew instead:"
	err "  brew install mcavage/tap/pix"
	err "Or pick a different PIX_PREFIX, e.g.:"
	err "  PIX_PREFIX=\$HOME/.local/bin sh install.sh"
	err ""
	err "Nothing was written."
	exit 1
}
```

Call `guard_homebrew_prefix` as the first line of `do_install` (before
`platform="$(detect_platform)"`), and also from `main()` before dispatch if
`--uninstall` targets a Homebrew prefix (same reasoning: don't let
`install.sh --uninstall` rm brew-owned files either).

**Acceptance criteria.**
- `PIX_DRYRUN=1 PIX_PREFIX=/opt/homebrew/bin sh install.sh` → exits nonzero,
  last line of stderr is exactly `Nothing was written.`, no file created
  under `/opt/homebrew` (there wasn't going to be one in dry-run anyway -
  also run without `PIX_DRYRUN` against a scratch fake "Homebrew" dir: `mkdir
  -p /tmp/fakebrew/Homebrew && PIX_PREFIX=/tmp/fakebrew sh install.sh` should
  still refuse via the path-literal branch since no real `brew` is on PATH
  there).
- Needs Mac: with real Homebrew installed, `PIX_PREFIX=$(brew --prefix)/bin
  sh install.sh` → refuses, mentions `brew install mcavage/tap/pix`.
- A plain `PIX_DRYRUN=1 sh install.sh` (default `~/.local/bin` prefix) is
  unaffected - still prints the dry-run report, exit 0.

---

## Step 4 - fix the two liars

**Needs a Mac** for the duplicate-PATH-entries manual check (P4 supplies the
real observed strings this step's copy should match); the Go logic itself is
unit-testable on Linux with fake PATH environments.

### 4a. `resolveBinPaths` / uninstall on Homebrew

**Files touched.** `services/host/cmd/pix/reset.go` (`resolveBinPaths` at
`reset.go:833-845`, `runUninstallCore` at `reset.go:900`, `removeBinSymlinks`
at `reset.go:850`).

Today `resolveBinPaths` unconditionally hardcodes `~/.local/bin/{pix,pix-host}`
(`reset.go:840-844`). On a Homebrew install this returns paths that don't
exist, so `removeBinSymlinks` reports "not installed" for both - a plausible
but false "success" while `brew` still owns real files elsewhere.

**Implementation sketch.**

```go
// resolveBinPaths returns the installed launcher paths pix should try to
// remove - but on a Homebrew install, pix itself must never touch them; only
// `brew uninstall` may. Consults the provenance detector so `pix uninstall`
// tells the truth about who owns these files.
func resolveBinPaths(env shellEnv) []string {
	home := ""
	if env.homeDir != nil {
		home = env.homeDir()
	}
	bin := filepath.Join(home, ".local", "bin")
	return []string{
		filepath.Join(bin, "pix"),
		filepath.Join(bin, "pix-host"),
	}
}

// homebrewUninstallNotice is what `pix uninstall` prints instead of trying
// to remove binaries, when provenance says Homebrew owns them. Returns ""
// when the running install isn't Homebrew (the existing removeBinSymlinks
// path applies as before).
func homebrewUninstallNotice(prov provenance, stdin io.Reader, out io.Writer, runBrew func() error) string {
	if prov.Channel != channelHomebrew {
		return ""
	}
	fmt.Fprintln(out, "Binaries + man page are owned by Homebrew, not pix.")
	fmt.Fprintln(out, "pix will NOT touch them here. Uninstall order matters:")
	fmt.Fprintln(out, "  1. pix state uninstall   (already ran - this stops/clears host state first)")
	fmt.Fprintln(out, "  2. brew uninstall mcavage/tap/pix")
	fmt.Fprintln(out, "Running step 2 out of order leaves a launchd job pointed at a binary that no")
	fmt.Fprintln(out, "longer exists, which retries forever with no visible symptom (see caveats).")
	return "brew uninstall mcavage/tap/pix"
}
```

Wire this into `runUninstallCore` (`reset.go:900`) ahead of the
`removeBinSymlinks(bins, fsys, rio.out)` call at `reset.go:938`: when
provenance is Homebrew, call `homebrewUninstallNotice` instead of
`removeBinSymlinks`, and (matching the Step 2 house style) offer to run
`brew uninstall mcavage/tap/pix` on a TTY, never on `--force`, never
unprompted.

### 4b. Duplicate-install detection on every PATH entry

**Files touched.** New: a `pathDuplicates` helper (put it in `provenance.go`
next to Step 1, since it is the same "where does this binary really live"
question); consumers: `status.go` (`gatherStatus` / `renderStatus`),
`doctor.go` (`runDoctor`).

Today nothing enumerates every `pix`/`pix-host` on PATH; `findHostBinary()`
only ever looks at the sibling-of-self, then the FIRST PATH hit
(`exec.LookPath`), and never compares that against `os.Executable()` for the
`pix` launcher itself (there's no self-shadow check for `pix`, only for
`pix-host`, and even that one never reports the additional entries it
skipped past).

**Implementation sketch.**

```go
// allOnPath returns every match for `name` across every PATH directory (not
// just exec.LookPath's first hit), so a shadow can be reported completely.
func allOnPath(name string, getenv func(string) string) []string {
	var found []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		found = append(found, p)
	}
	return found
}

// pathShadowIssue reports a non-empty message when the RUNNING binary is not
// the first `name` on PATH - i.e. this process is not what a plain `pix` or
// `pix-host` invocation would resolve to. Never deletes anything; this is
// report-only.
func pathShadowIssue(name string, self string, getenv func(string) string) string {
	all := allOnPath(name, getenv)
	if len(all) == 0 {
		return ""
	}
	first, err := filepath.EvalSymlinks(all[0])
	if err != nil {
		first = all[0]
	}
	selfResolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		selfResolved = self
	}
	if first == selfResolved {
		return "" // no shadow - the running binary IS what PATH resolves to
	}
	msg := fmt.Sprintf("%s on PATH resolves to %s, not the running binary (%s).", name, all[0], self)
	msg += fmt.Sprintf("\n  fix: put the intended install ahead on PATH, or remove the stale one:\n    rm %s   # only if you no longer want this copy\n", all[0])
	if len(all) > 2 {
		msg += fmt.Sprintf("  (also found at: %s)\n", strings.Join(all[2:], ", "))
	}
	return msg
}
```

Wire into `status.go`'s `gatherStatus`/`render` (bare `pix` runs this - it's
the first thing a confused user sees per the story) and `doctor.go`'s
`runDoctor`: call `pathShadowIssue("pix", exePath, env.getenv)` and
`pathShadowIssue("pix-host", hostBinPath, env.getenv)`, surface as a hard
warning line, never auto-fix.

**Acceptance criteria.**
- `go test ./services/host/cmd/pix/ -run TestResolveBinPaths -v` and
  `TestPathShadowIssue` (fake PATH with 2-3 fake `pix` files via `t.TempDir`,
  assert the message names both/all paths and the fix is non-destructive -
  i.e. assert the string never contains `rm -rf` or targets `self`).
- `go test ./services/host/cmd/pix/ -run TestRunUninstall -v` extended:
  Homebrew-channel fake shows the "owned by Homebrew" message and does NOT
  call `removeBinSymlinks`.
- Needs Mac (uses P4's observed output): reproduce P4's shadow scenario, run
  `pix` bare and `pix doctor`, confirm the printed shadow message matches
  what this step's acceptance test asserts, i.e. the plan's copy was written
  against reality, not guessed.

---

## Step 5 - collapse the kit/image pin

**Needs a Mac:** no. This is pure Go + one YAML precedence rule; verifiable
entirely from a Linux checkout (`pix run` itself needs sbx/Docker to fully
exercise, but the ref-resolution logic does not).

**Goal.** Kit and image pin to the launcher's own stamped version, always
(modulo the two overrides). Delete the runtime "latest stable" HTTP lookup
and its 24h cache. `pix run` never makes a network call for this. New
precedence: `--kit-ref` > `version_pin` > stamped version (three levels, was
four).

**Files touched.** `services/host/cmd/pix/kitref.go` (delete the caching +
fetch machinery, rewrite `resolveKitRef` + the header comment), whatever
calls `resolveLatestRelease` / `kitRefNotice` from `run.go` (grep first -
likely `runRun` in the run-verb file), delete `kitref_test.go` cases that
exercised the deleted cache/fetch paths, keep cases that exercise
`resolveKitRef`/`normalizeKitRef`.

**Implementation sketch.** Delete: `latestReleaseTTL`, `latestReleaseTimeout`,
`releasesLatestURL`, `latestReleaseCache` (struct), `latestReleaseCachePath`,
`readLatestReleaseCache`, `writeLatestReleaseCache`,
`parseLatestReleaseLocation`, `fetchLatestRelease`, `resolveLatestRelease`,
`kitRefLatest` (the enum value), and the `kitRefLatest` case in
`kitRefNotice`. Also delete `upgrade.go`'s reuse of
`writeLatestReleaseCache`/`fetchLatestRelease`/`releasesLatestURL` (the
"refresh the memo while we're here" block in `runUpgrade`) since the cache no
longer exists - `pix upgrade`'s own "ask GitHub directly" lookup for the
Installer channel is unaffected (it already calls
`fetchLatestRelease`... which is being deleted here - replace that one
in-Step-2 call site with an inline HEAD-request-and-parse using the same
`parseLatestReleaseLocation` logic folded directly into `upgrade.go`, since
`pix upgrade --version`-less on the Installer channel still needs to know
"what's the latest tag", it just doesn't cache it across runs the way
`pix run` used to).

Rewrite what's left:

```go
// resolveKitRef applies the (now three-level) precedence chain and reports
// which rule won: --kit-ref, then version_pin in config.toml, then this
// binary's own stamped version. There is no runtime "latest stable" lookup
// any more - see the header comment for why that's now safe to remove.
func resolveKitRef(version, flagRef, pinRef string) (ref string, src kitRefSource) {
	switch {
	case flagRef != "":
		return flagRef, kitRefFlag
	case pinRef != "":
		return normalizeKitRef(pinRef), kitRefConfigPin
	case !isReleased(version):
		return "", kitRefUnreleased
	default:
		return "", kitRefStamped
	}
}
```

Rewrite the header comment (do not delete the rationale - the WHY it existed
matters, and the new WHY it's gone is the actual interesting content):

```go
// kitref.go - which release does `pix run` actually run?
//
// This USED to resolve the latest stable release at run time (24h cache,
// 2s network budget, silent fallback on any failure). That existed because
// pinning the kit to the launcher's OWN stamped version forever meant a
// consumer who installed once was frozen there - 0.1.1 shipped an image that
// could not boot at all, and a pinned launcher had no way to notice.
//
// Homebrew removes the reason that machinery existed. `brew upgrade
// mcavage/tap/pix` is now the ambient, user-initiated update channel (see
// upgrade.go) - the fix for "stuck on a broken pin" is "run the upgrade you
// already know to run", not "make every `pix run` phone home to check".
// So the launcher goes back to pinning the kit/image to its OWN stamped
// version, always - a `pix run` makes zero network calls for this, and the
// kit/image are no longer an UNATTENDED update channel a user never asked
// for. Two escape hatches remain for when you need something else RIGHT NOW:
//
//   1. --kit-ref REF     one-off override for this invocation
//   2. version_pin       persistent override in config.toml
//
// Precedence, high to low: --kit-ref, version_pin, the stamped version.
```

**Acceptance criteria.**
- `go test ./services/host/cmd/pix/ -run TestResolveKitRef -v` - updated
  table drops the `latest` param and the `kitRefLatest` case entirely;
  three cases remain (flag wins, pin wins, stamped default), plus
  `kitRefUnreleased` for a dev build.
- `grep -rn "latestReleaseTTL\|fetchLatestRelease\|resolveLatestRelease\|releasesLatestURL" services/host/cmd/pix/kitref.go` → no matches (fully removed from this file).
- `go build ./services/host/...` - compiles clean (proves nothing else still
  references the deleted symbols; grep the whole `cmd/pix` package for any
  remaining reference before deleting, since `upgrade.go` reused three of
  them).
- Behavioral: `strace`/`dtrace` is overkill - simplest proof is a unit test
  asserting `resolveKitRef` never takes a `latest` argument any more (the
  signature change itself is the proof no network path remains reachable
  from it), plus a grep across `run.go` confirming no call site still passes
  a `latest` value in.

---

## Step 6 - combined tarball

**Needs a Mac:** no, for building/verifying the tarball contents - the CI
runner is `ubuntu-latest` cross-compiling `GOOS=darwin`. The eventual "does
this actually run on a Mac" check is P1/P2's job, already done earlier.

**Goal.** Add `pix_<version>_darwin_<arch>.tar.gz` (both binaries) for
arm64+amd64, additive to today's loose `pix-<os>-<arch>` assets. Fix the
wrong CI comment about `pix-host`'s version symbol.

**Files touched.** `.github/workflows/publish.yml` (`release-binaries` job,
"Cross-compile both binaries" step and the comment above it).

**Implementation sketch.** The current comment
(`.github/workflows/publish.yml`, "Cross-compile both binaries" step) says:

```
# -X main.version stamps the launcher (its `var version`); the host binary has
# no such symbol, so the linker silently ignores the flag there - harmless, and
# keeps one build recipe for both.
```

This is wrong: `services/host/main.go:46` has `var version = "dev"` too, and
the SAME `-X main.version=$V` ldflags already stamps it (both binaries build
`package main` in the same module, same symbol name). Fix the comment, then
add the tarball step after the existing loose-binary loop:

```yaml
      - name: Cross-compile both binaries
        working-directory: services/host
        env:
          V: ${{ needs.version.outputs.version }}
        run: |
          set -eu
          mkdir -p "$GITHUB_WORKSPACE/dist"
          for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
            os="${target%/*}"; arch="${target#*/}"
            echo "building $os/$arch"
            # -X main.version stamps BOTH binaries: the launcher's `var version`
            # (cmd/pix/main.go) and pix-host's own `var version`
            # (services/host/main.go:46) - same symbol name, same module, one
            # ldflags recipe covers both.
            GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
              go build -ldflags "-X main.version=$V" \
              -o "$GITHUB_WORKSPACE/dist/pix-$os-$arch" ./cmd/pix
            GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
              go build -ldflags "-X main.version=$V" \
              -o "$GITHUB_WORKSPACE/dist/pix-host-$os-$arch" ./
          done

      - name: Build combined darwin tarballs (for the Homebrew formula)
        working-directory: dist
        env:
          V: ${{ needs.version.outputs.version }}
        run: |
          set -eu
          for arch in amd64 arm64; do
            stage="$(mktemp -d)"
            cp "pix-darwin-$arch" "$stage/pix"
            cp "pix-host-darwin-$arch" "$stage/pix-host"
            chmod +x "$stage/pix" "$stage/pix-host"
            tar -C "$stage" -czf "pix_${V}_darwin_${arch}.tar.gz" pix pix-host
            rm -rf "$stage"
          done
          ls -la pix_*.tar.gz

      - name: Generate SHA256SUMS
        working-directory: dist
        run: sha256sum pix-* pix_*.tar.gz > SHA256SUMS && cat SHA256SUMS

      - name: Upload assets to the v<version> release
        uses: softprops/action-gh-release@3bb12739c298aeb8a4eeaf626c5b8d85266b0e65 # v2.6.2
        with:
          tag_name: v${{ needs.version.outputs.version }}
          name: pix v${{ needs.version.outputs.version }}
          files: |
            dist/pix-*
            dist/pix_*.tar.gz
            dist/SHA256SUMS
          fail_on_unmatched_files: true
```

Note `sha256sum pix-*` in the existing "Generate SHA256SUMS" step already
globs the loose per-binary assets; widen it to also catch `pix_*.tar.gz`
(shown above) - do NOT run `sha256sum` twice into two files, one SHA256SUMS
must contain every asset, since `install.sh`'s `verify()` and the new
formula's checksum check both read the one file.

**Acceptance criteria.**
- Local dry run (no Mac needed): `cd services/host && GOOS=darwin
  GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/dist/pix-darwin-arm64
  ./cmd/pix && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o
  /tmp/dist/pix-host-darwin-arm64 ./` then reproduce the tarball step by
  hand; `tar tzf /tmp/dist/pix_test_darwin_arm64.tar.gz` → exactly `pix` and
  `pix-host`, both executable (`tar tzvf ... | grep -c '^-rwx'` → `2`).
- `cat dist/SHA256SUMS` after a real CI run (or the local reproduction)
  lists both loose binaries AND both tarballs - 4 darwin loose assets (2
  arch × pix/pix-host) + 2 linux loose × 2 + 2 tarballs = 10 lines total for
  a full run.
- Regression (this is P3, formalized as CI): a fresh checkout's
  `install.sh` against `PIX_VERSION=<this test version>
  PIX_PREFIX=/tmp/scratch sh install.sh` still succeeds using ONLY the loose
  assets - the tarball must never replace them.
- `grep -n "no such symbol" .github/workflows/publish.yml` → no matches (the
  wrong comment is gone).

---

## Step 7 - the formula

**Needs a Mac:** yes, to actually run `brew install` / `brew test` / `brew
audit` against it (Homebrew itself doesn't run on Linux). P1 already
prototyped the shape; this step formalizes it with real checksums wired to
Step 6's release assets and Step 8's automation.

**Goal.** ~35 line formula in the new `mcavage/homebrew-tap` repo. Zero
dependencies. `on_macos`/`on_arm`/`on_intel`, immutable per-tag URLs
(never `releases/latest/download`), installs both binaries + the manpage,
a `livecheck` block, and a one-sentence `caveats` naming `pix setup` plus
the uninstall order.

**Files touched.** New repo `mcavage/homebrew-tap`, file
`Formula/pix.rb`.

**Implementation sketch.**

```ruby
class Pix < Formula
  desc "Multi-model coding agent harness for Docker Sandboxes"
  homepage "https://github.com/mcavage/pix"
  version "0.9.0" # kept in lockstep by the Step 8 bump job - never hand-edit
  license "MIT" # confirm against the actual repo LICENSE before merging

  # No `depends_on`. `pix setup` owns op/sbx/ollama/gh - see README + the
  # decision doc; this formula's job is exactly two files + a manpage.

  on_macos do
    on_arm do
      url "https://github.com/mcavage/pix/releases/download/v0.9.0/pix_0.9.0_darwin_arm64.tar.gz"
      sha256 "REPLACE_WITH_REAL_SHA256_ARM64"
    end
    on_intel do
      url "https://github.com/mcavage/pix/releases/download/v0.9.0/pix_0.9.0_darwin_amd64.tar.gz"
      sha256 "REPLACE_WITH_REAL_SHA256_AMD64"
    end
  end

  def install
    bin.install "pix", "pix-host"
    man1.install "pix.1" if File.exist?("pix.1")
    # NOTE: pix.1 is NOT currently in the release tarball (Step 6 only packs
    # the two binaries). Either add pix.1 to the tarball in Step 6, or fetch
    # it from the tagged source tree here via `resource` - pick one before
    # merging this formula; do not ship a formula that silently skips the
    # man page. Recommended: add it to Step 6's tarball, since a `resource`
    # block means a second URL + sha256 to keep in lockstep.
  end

  livecheck do
    url :stable
    strategy :github_latest
  end

  # Shell completions do NOT exist: pix dispatches from a hand-written verb
  # switch (services/host/cmd/pix/main.go), not Cobra/urfave - there is
  # nothing to generate or install here. Do not add a completions line.

  def caveats
    <<~EOS
      Run `pix setup` to install prerequisites (op, sbx, ollama, gh) and finish onboarding.

      To uninstall: run `pix state uninstall` FIRST, then `brew uninstall mcavage/tap/pix`.
      Doing it in the other order leaves a launchd job pointed at a binary that no
      longer exists, and it will retry forever with no visible error.
    EOS
  end

  test do
    # The fitness function for the whole two-binary stamping invariant: if
    # either binary's ldflags stamp broke, or the tarball packed a mismatched
    # pair, this fails here instead of at a user's `pix run`.
    assert_equal version.to_s, shell_output("#{bin}/pix version").strip
    assert_equal version.to_s, shell_output("#{bin}/pix-host version").strip
  end
end
```

**Acceptance criteria.**
- Needs Mac: `brew install --build-from-source=false
  mcavage/tap/pix` (against a real tagged release once Step 6 ships) on both
  arm64 and intel → succeeds, `pix version` and `pix-host version` at
  `$(brew --prefix)/bin/` print the formula's `version`.
- `brew test mcavage/tap/pix` → passes (this is the `test do` block above,
  run standalone).
- `brew audit --strict --online mcavage/tap/pix` (also Step 9's CI gate) →
  no errors.
- `man pix` after install → renders (confirms `man1.install` worked; resolve
  the `pix.1`-in-tarball question above before this can be verified for
  real).
- `wc -l Formula/pix.rb` → around 35 lines, matching the story's own budget
  (a formula that balloons past this is a signal something optional crept
  in - dependencies, completions, post-install hooks - any of which
  violates a closed decision above).

---

## Step 8 - auto-bump

**Needs a Mac:** no. This is a GitHub Actions job; the runner is
`ubuntu-latest` regardless of what it's building for.

**Goal.** A job in `publish.yml`, gated on `release-binaries`, that commits
the new version + checksums to the tap. Reads digests FROM `SHA256SUMS`
(never re-downloads and re-hashes - that would launder a stronger control
into a rubber stamp). Credential scoped to the tap repo only.

**Files touched.** `.github/workflows/publish.yml` (new `bump-tap` job),
tap repo needs a receiving mechanism (a plain `git push` to `main`, or a
branch - see Step 9 for why a branch is actually required).

**Implementation sketch.**

```yaml
  bump-tap:
    name: bump the Homebrew tap formula
    needs: [version, release-binaries]
    runs-on: ubuntu-latest
    steps:
      - name: Download this release's SHA256SUMS
        run: |
          curl -fsSL -o SHA256SUMS \
            "https://github.com/mcavage/pix/releases/download/v${{ needs.version.outputs.version }}/SHA256SUMS"

      - name: Extract digests - READ FROM SHA256SUMS, never re-hash the asset
        id: sha
        run: |
          set -euo pipefail
          V="${{ needs.version.outputs.version }}"
          arm="$(awk -v f="pix_${V}_darwin_arm64.tar.gz" '$2==f{print $1}' SHA256SUMS)"
          amd="$(awk -v f="pix_${V}_darwin_amd64.tar.gz" '$2==f{print $1}' SHA256SUMS)"
          test -n "$arm" && test -n "$amd"
          echo "arm64=$arm" >> "$GITHUB_OUTPUT"
          echo "amd64=$amd" >> "$GITHUB_OUTPUT"

      - name: Checkout the tap
        uses: actions/checkout@34e114876b0b11c390a56381ad16ebd13914f8d5 # v4.3.1
        with:
          repository: mcavage/homebrew-tap
          token: ${{ secrets.TAP_PUSH_TOKEN }} # scoped to homebrew-tap ONLY - see below
          path: tap

      - name: Write the bumped formula
        env:
          V: ${{ needs.version.outputs.version }}
          ARM_SHA: ${{ steps.sha.outputs.arm64 }}
          AMD_SHA: ${{ steps.sha.outputs.amd64 }}
        run: |
          set -euo pipefail
          cd tap
          sed -i -E "s/version \"[^\"]+\"/version \"$V\"/" Formula/pix.rb
          sed -i -E "0,/sha256 \"[^\"]+\"/s//sha256 \"$ARM_SHA\"/" Formula/pix.rb
          # second occurrence is the intel line - a small ruby/python script is
          # more robust than double-sed here; sketch only, harden before shipping.
          python3 - <<'PY'
          import re, os
          path = "Formula/pix.rb"
          s = open(path).read()
          # replace url lines for arm/intel blocks explicitly, and both sha256 lines
          s = re.sub(r'(on_arm do\n\s*url "[^"]+"\n\s*sha256 ")[^"]+(")', r'\1' + os.environ["ARM_SHA"] + r'\2', s)
          s = re.sub(r'(on_intel do\n\s*url "[^"]+"\n\s*sha256 ")[^"]+(")', r'\1' + os.environ["AMD_SHA"] + r'\2', s)
          s = re.sub(r'download/v[0-9.]+/pix_[0-9.]+_darwin_arm64', f'download/v{os.environ["V"]}/pix_{os.environ["V"]}_darwin_arm64', s)
          s = re.sub(r'download/v[0-9.]+/pix_[0-9.]+_darwin_amd64', f'download/v{os.environ["V"]}/pix_{os.environ["V"]}_darwin_amd64', s)
          open(path, "w").write(s)
          PY

      - name: Assert the formula's checksums match SHA256SUMS (release-time check)
        run: |
          set -euo pipefail
          grep -q "${{ steps.sha.outputs.arm64 }}" tap/Formula/pix.rb
          grep -q "${{ steps.sha.outputs.amd64 }}" tap/Formula/pix.rb

      - name: Push a branch, not main (tap CI must gate the merge - see Step 9)
        working-directory: tap
        run: |
          set -euo pipefail
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git checkout -b "bump-v${{ needs.version.outputs.version }}"
          git add Formula/pix.rb
          git commit -m "pix v${{ needs.version.outputs.version }}"
          git push origin "bump-v${{ needs.version.outputs.version }}"
          gh pr create --repo mcavage/homebrew-tap \
            --title "pix v${{ needs.version.outputs.version }}" \
            --body "Automated bump from mcavage/pix release." \
            --base main --head "bump-v${{ needs.version.outputs.version }}"
        env:
          GH_TOKEN: ${{ secrets.TAP_PUSH_TOKEN }}
```

**Credential.** `TAP_PUSH_TOKEN` must be a fine-grained PAT (or a GitHub App
installation token) with `contents:write` + `pull-requests:write` scoped to
`mcavage/homebrew-tap` ONLY. Never a classic `repo`-scope PAT - that
credential, if it ever leaked from `mcavage/pix`'s Actions secrets, would
hand over every repo the account owns, not just the tap. Set this up on the
Mac/GitHub-UI side (fine-grained PATs and App installs are configured in the
GitHub UI, not something the agent can do headlessly without the account
owner).

**Acceptance criteria.**
- `gh secret list --repo mcavage/pix` shows `TAP_PUSH_TOKEN` (not `GH_TOKEN`,
  not a repo-wide PAT reused from elsewhere - verify by checking the PAT's
  scopes in GitHub's token settings UI before wiring it in, since Actions
  cannot introspect a secret's own scope).
- A real push to `main` on `mcavage/pix` results in a new PR on
  `mcavage/homebrew-tap` titled `pix v<version>` whose diff touches ONLY
  `version`, the two `url` lines, and the two `sha256` lines.
- `diff <(curl -fsSL .../v<version>/SHA256SUMS | awk '/darwin_arm64/{print $1}') <(grep -A2 on_arm tap/Formula/pix.rb | grep sha256 | grep -oE '[0-9a-f]{64}')`
  → empty (the release-time check step, run standalone).
- Deliberately corrupt one digest in the generated formula in a test run →
  the "Assert the formula's checksums match SHA256SUMS" step fails loud,
  proving the check isn't a no-op.

---

## Step 9 - tap CI

**Needs a Mac:** no to write; the tap's own CI runs on `macos-14` /
`macos-13` GitHub-hosted runners, which is how you get Mac coverage on
every PR without owning a Mac yourself. (A real physical Mac is still
needed for the Step 7/P1 exploratory work; this step is the ongoing
regression gate afterward.)

**Goal.** `brew audit --strict --online`, a real install + smoke on
`macos-14` (arm) and `macos-13` (intel). The bump PR from Step 8 must be
green here before it fast-forwards `main`. Add an anti-drift check the
repo-local lockstep tests structurally cannot do (they can't see across
repos): compare the formula version against GitHub's `releases/latest`
redirect.

**Files touched.** New: `mcavage/homebrew-tap/.github/workflows/ci.yml`.

**Implementation sketch.**

```yaml
name: ci
on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        os: [macos-14, macos-13] # arm64, intel
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@34e114876b0b11c390a56381ad16ebd13914f8d5 # v4.3.1
      - name: brew audit
        run: brew audit --strict --online Formula/pix.rb
      - name: Install from this checkout's formula
        run: |
          brew install --formula ./Formula/pix.rb
      - name: Smoke - both binaries report the SAME formula version
        run: |
          v="$(brew info --json=v2 --formula pix | jq -r '.formulae[0].versions.stable')"
          test "$(pix version)" = "$v"
          test "$(pix-host version)" = "$v"
      - name: brew test
        run: brew test pix

  anti-drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@34e114876b0b11c390a56381ad16ebd13914f8d5 # v4.3.1
      - name: Formula version must match pix's latest GitHub release
        run: |
          set -euo pipefail
          formula_v="$(grep -oE 'version "[0-9.]+"' Formula/pix.rb | grep -oE '[0-9.]+')"
          latest_tag="$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/mcavage/pix/releases/latest)"
          latest_v="${latest_tag##*/tag/v}"
          if [ "$formula_v" != "$latest_v" ]; then
            echo "::error::formula pins $formula_v, mcavage/pix latest release is $latest_v"
            exit 1
          fi
```

Note this anti-drift job will always be red on a bump PR itself (the PR
exists precisely because they differ, briefly) - gate it to run on `push:
main` only, or accept it's expected-red on the bump PR and only required on
`main`. Decide which when writing the actual workflow; sketch above needs
that branch condition added before it's real.

**Acceptance criteria.**
- `brew audit --strict --online Formula/pix.rb` locally reproducible on any
  Mac with the tap checked out.
- A bump PR from Step 8 shows required checks `test (macos-14)`, `test
  (macos-13)` green before it can fast-forward `main` (branch protection on
  `mcavage/homebrew-tap`'s `main` requires those two contexts - configure in
  repo settings, not in the workflow file).
- `anti-drift` on `main` passes immediately after a successful bump merge,
  fails if `main` is ever hand-edited out of sync with the latest pix
  release.

---

## Step 10 - docs

**Needs a Mac:** no.

**Goal.** README down to three commands, fully-qualified `brew install
mcavage/tap/pix` (never bare `brew install pix` - Homebrew's own error for
that is "No available formula with the name pix. Did you mean pixi?", worth
quoting in the doc so a future editor doesn't re-introduce the bare form).
install.sh demoted to a footnote. `onboarding-v3.md` section 3 corrected
(wrong tap name, wrong "formula owns dependencies" claim). The sbx install
hint collapsed into one Go constant.

**Files touched.** `README.md` (lines ~13-60, the whole "Install and run"
section), `docs/design/onboarding-v3.md` (section 3, currently
`brew install docker/tap/pix` and "The Homebrew formula owns binaries and
ordinary package dependencies"), new: a shared Go constant for the sbx
install hint, referenced from `doctor.go`, `run.go`, `setup.go`.

**Implementation sketch - README** (replacing the current four-command
"Install and run" section):

```markdown
## Install and run

```bash
brew install mcavage/tap/pix
pix setup
pix run
```

`pix setup` installs prerequisites (`sbx`, `op`, `ollama`, `gh`) and walks
through onboarding. Don't use the bare `brew install pix` - Homebrew doesn't
know a formula by that name (it'll offer you `pixi` instead); the tap
qualifier `mcavage/tap/` is required.

<details>
<summary>Alternative: curl installer (Linux, or no Homebrew)</summary>

```bash
curl -fsSL https://raw.githubusercontent.com/mcavage/pix/main/install.sh | sh
```

This is the fallback path. On macOS, prefer Homebrew: `pix upgrade` on a
curl-installed Mac is a fully manual re-run of this script; on a
Homebrew-installed Mac, `pix upgrade` just tells you to run `brew upgrade
mcavage/tap/pix`.
</details>
```

**Implementation sketch - sbx install-hint constant.** New file (or add to
an existing shared-constants file - check for one first; if none exists,
create `services/host/cmd/pix/hints.go`):

```go
package main

// hints.go - user-facing strings that would otherwise be copy-pasted across
// doctor/run/setup and drift. sbxInstallHint is the one that matters most
// right now: `sbx` currently ships as `docker/tap/sbx@nightly` and is
// expected to rename within weeks once its MCP support stabilizes. When that
// happens, this is the ONLY line that needs to change.
const sbxInstallHint = "brew install docker/tap/sbx@nightly"
```

Then replace every literal `"brew install docker/tap/sbx@nightly"` in
`doctor.go`/`run.go`/`setup.go`/anywhere else it's inlined with
`sbxInstallHint`. `README.md`'s copy of the same string is NOT sourced from
this constant (README isn't Go), so note in the constant's comment that
README also needs a one-line edit on the eventual rename - that's an
accepted, documented exception, not a second scattered copy inside Go.

**onboarding-v3.md fix** (section 3, currently at the lines read from the
file):

```diff
-Make Homebrew the primary macOS distribution. Keep the signed release installer as a fallback.
+Homebrew is the primary and only supported macOS distribution. The curl
+installer (`install.sh`) is a fallback for Linux or a machine without
+Homebrew; there is no signed-release path.

-The Homebrew formula owns binaries and ordinary package dependencies. `pix setup` owns stateful work: authentication, configuration, model selection, service lifecycle, pack activation, and verification. Do not turn the shell downloader into a second setup engine.
+The Homebrew formula owns exactly two binaries and a man page. It declares
+ZERO dependencies. `pix setup` owns every prerequisite (`op`, `sbx`,
+`ollama`, `gh`) and all stateful work: authentication, configuration, model
+selection, service lifecycle, pack activation, and verification. Do not
+turn the shell downloader into a second setup engine.

 The supported path becomes:

 ```bash
-brew install docker/tap/pix
+brew install mcavage/tap/pix
 pix setup --pack docker/gm-pix-pack
 pix run
 ```
```

**Acceptance criteria.**
- `grep -rn "docker/tap/pix" README.md docs/design/onboarding-v3.md` → no
  matches (the wrong tap name is gone everywhere).
- `grep -rn "mcavage/tap/pix" README.md docs/design/onboarding-v3.md` → at
  least one match each.
- `grep -c "brew install docker/tap/sbx@nightly" services/host/cmd/pix/*.go`
  → exactly the ONE occurrence inside `hints.go`'s constant definition; every
  other `.go` file references `sbxInstallHint` instead
  (`grep -rn "docker/tap/sbx@nightly" services/host/cmd/pix/*.go | grep -v hints.go` → empty).
- `readme_test.go:55` (`strings.Contains(body, "brew install
  docker/tap/sbx@nightly")`) still passes unmodified - this test is about the
  sbx tap name, not pix's own, so Step 10 must NOT touch that assertion; it's
  a separate rename tracked by the "2 more weeks" note in the ORDERED WORK
  section, out of scope here.
- `go build ./services/host/...` after the constant extraction - proves
  every call site compiles against the new shared symbol.

---

## How to verify you are done

Run these, in order, on a Mac (arm64 and intel where noted), starting from a
clean `~` with no prior pix install:

1. `brew install mcavage/tap/pix && pix version && pix-host version` - both
   print the same version, matching `brew info --formula pix`'s version.
2. `man pix` - renders.
3. `pix setup` - completes without brew-owned prereqs (op/sbx/ollama/gh)
   reported as "formula should have installed this"; it installs/prompts for
   them itself.
4. `pix run` (any dir) - no network call for kit resolution (verify with
   `dtrace`/Little Snitch/a packet capture on the release-fetch domain
   during the run, or simplest: confirm `kitref.go` no longer contains any
   HTTP client construction - the Step 5 acceptance criteria's grep already
   proves this statically).
5. Bump `mcavage/pix`'s version (a real push to `main`, or a manual
   `workflow_dispatch`) → within minutes, a PR appears on
   `mcavage/homebrew-tap` with only version/url/sha256 changed, tap CI goes
   green on both macos-14 and macos-13, and it merges (auto or by hand,
   depending on how branch protection is finally configured in Step 9).
6. `brew upgrade mcavage/tap/pix` picks up that bump; `pix upgrade` on this
   machine detects Homebrew, prints the exact `brew upgrade
   mcavage/tap/pix` line, and after consent + run, prints `verified: pix
   <new version>` - never a bare "upgraded" without that probe.
7. Reproduce the shadow scenario (curl-install an old `pix` to
   `~/.local/bin` first, then brew-install): bare `pix` and `pix doctor` both
   show the shadow warning, name both paths, and the suggested fix never
   deletes anything by itself.
8. `pix state uninstall` then `brew uninstall mcavage/tap/pix` - no
   orphaned `launchd` job left retrying (confirm via `launchctl list | grep
   pix` - empty, or absent if `serve install` was never run in this pass).
   Reversing the order (`brew uninstall` first) is the documented-bad path
   from the caveats block - confirm the caveats text is the thing that would
   have prevented it, i.e. it's legible BEFORE the mistake, not just an
   after-the-fact explanation.
9. `grep -rn "docker/tap/pix\b" .` across the whole repo (excluding
   `.git`) → zero matches. `grep -rn "brew install pix\b"` (the bare,
   unqualified form) → zero matches outside of an explicit "don't do this"
   callout.
10. `go test ./services/host/... ` - all green, including the new
    provenance/upgrade-router/shadow tests and the untouched
    `versionlockstep_test.go` / `release_convention_test.go` (still passing,
    still blind to the tap's fourth version copy - that blindness is
    accepted, covered instead by Step 9's anti-drift job in the OTHER repo).
