# Homebrew distribution

Status: DECIDED. Implementation plan in `homebrew-distribution-build.md`.

Make Homebrew the primary and only supported macOS distribution for pix, and
collapse the update layers so one `brew upgrade` moves everything.

## Decisions

| | Decision |
|---|---|
| Platform | macOS only. Linux binaries keep building (free), but it is not a supported path |
| Tap | `mcavage/homebrew-tap` → `brew install mcavage/tap/pix` |
| Package | Formula, prebuilt binaries. Not a cask, not build-from-source |
| Shape | One formula, both binaries, one tarball per arch |
| Dependencies | The formula declares none. `pix setup` owns prereqs |
| `pix upgrade` | Stays, shrinks to a provenance router |
| Update layers | Collapsed. Kit and image pin to the launcher's stamped version |
| Audience | Public-first |
| Release gate | Push equals shipped, unchanged for now |
| goreleaser | No |

Tap naming trap: the repo must be named `homebrew-tap`. A repo named
`homebrew-pix` yields `brew install mcavage/pix/pix`.

## Why, in short

The install step was never the friction. `install.sh` is already one command that
does arch detection, per-asset sha256 verification, and a staged atomic two-binary
install. `brew install` is also one command.

What brew actually buys:

1. **Ambient upgrades.** `brew upgrade` is a habit people already have. `pix
   upgrade` is a command nobody schedules.
2. **A deleted failure class.** No prefix selection, no `~/.local/bin` missing from
   PATH, no two-binary skew window.
3. **The collapse.** This is the real prize, and it is only available because of
   point 1. See below.

## The collapse

`kitref.go` resolved the latest stable kit and image at run time instead of pinning
to the launcher's version, because 0.1.1 shipped an image that could not boot and a
pinned launcher would have served it forever.

**That reasoning was conditional on the binary being hard to update.** Brew makes
updating ambient, so pinning is safe again. Pinning the kit and image to the
launcher's stamped version means `brew upgrade` moves all of it in one motion.

Three side effects worth naming:

- `pix run` stops making a network call and stops needing a 24h cache.
- The kit and image stop being an unattended update channel that can change under
  you within 24 hours. That closes most of a real security finding: the kit's
  `spec.yaml` controls `credentials[].apiKey.inject[]`, so a malicious kit could
  add its own domain to the egress allowlist and the inject list and take every
  provider key on the next model call. It now only moves when the user runs
  `brew upgrade`.
- Layers go from three to two. Packs stay separate, because they are private git
  repos with a different owner and lifecycle. `git pull` is the right answer there.

Escape hatches survive. Precedence becomes `--kit-ref` > `version_pin` > stamped
version.

## Why the formula declares no dependencies

Formulae can only depend on formulae. Two of the required deps are casks.

| Dep | Kind | Declarable from a formula? |
|---|---|---|
| ollama | core formula | Yes |
| gh | core formula | Yes |
| sbx | another tap's formula | Only fully qualified, and brew does not auto-tap |
| 1password-cli | cask | **No** |
| Docker Desktop | cask | **No** |

And for the two that are declarable, installing the bytes gets nowhere: ollama
needs `brew services start` plus roughly 6GB of model pulls, gh needs `gh auth
login` plus `sbx secret set`. `depends_on` cannot express optional, cannot explain
why, and cannot be declined, so a fat formula becomes ten silent minutes of
downloads the user did not ask for, ending with `pix doctor` still saying not
ready. That is a worse first impression than curl|sh, which is honest about doing
nothing but dropping two files.

`sbx` is the one genuinely required dep and still should not be declared. It is on
`docker/tap/sbx@nightly` only until MCP functionality stabilizes, so **the formula
name itself is about to change.** Baked into a formula that is a bump plus a broken
install for anyone upgrading at the wrong moment. The install hint lives in one Go
constant shared by `doctor`, `run`, and `setup`.

## Why a formula and not a cask

Homebrew's written policy says binary-only distributions belong in cask, but that
is a **homebrew-core** rule and nothing enforces it in a personal tap. Worst case is
a `brew audit --strict` grumble nobody sees.

The cask branch is actively worse here: casks deliberately apply
`com.apple.quarantine`, which SIGKILLs an unsigned Go binary on first run with exit
137 and no error message. The documented workaround is a `postflight` running
`xattr -d com.apple.quarantine`, i.e. shipping a Gatekeeper bypass for our own
binaries. The formula path avoids it entirely, because brew fetches with curl,
which never sets the attribute.

Revisit only if `brew audit` starts hard-failing binary formulae, or if we ever
take on signing and notarization.

## Why not goreleaser

Re-evaluated after the macOS-only decision, and the answer got clearer, not
blurrier.

goreleaser's Homebrew publisher only targets **casks** going forward: `brews:` was
soft-deprecated at v2.10 and hard-deprecated at v2.16, replaced by
`homebrew_casks:` specifically for precompiled binaries. So using goreleaser for
the one step we wanted it for would push us onto the cask branch we just rejected,
or onto a hard-deprecated path.

It also wants to own tagging, changelog, and release creation. All three are
already owned by the increment-the-patch-until-the-tag-is-unused scheme, which
exists because sbx caches materialized templates per tag name. And macOS-only
halves the cross-build matrix, which was goreleaser's main draw. What is left is
tarballs plus checksums, roughly 15 lines of shell, against a second release system
with a second source of truth for the version string, guarded by a
`versionlockstep_test.go` written to prevent exactly that.

Revisit if we ever want signing and notarization, where its support is genuinely
good.

## Why `pix upgrade` survives

Not sentiment. **You cannot delete it without writing the brew-detection code
anyway**, because `install.sh` must refuse to write into a Homebrew prefix or it
corrupts the Cellar. Once the detector exists, the router is about ten more lines.
Deleting the verb saves nothing and leaves `command not found` for everyone who has
it in muscle memory.

It becomes: Homebrew channel prints the exact `brew upgrade` command and offers
`[y/N]`; anything else keeps today's behavior. It never writes into a prefix a
package manager owns.

Prior art: `uv self update` hit the identical conflict and resolved it by detecting
the install source and refusing, not by deleting the self-updater.

## The bugs this fixes on the way in

Found during planning, all verified against source. Each is live today.

1. **`pix upgrade` corrupts a brew install.** `installPrefix()` (`upgrade.go:109`)
   returns the running binary's directory, so on a brew install it runs install.sh
   with `PIX_PREFIX=$(brew --prefix)/bin` and overwrites brew's symlinks with real
   files. The user then sees a version that flips back and forth days later with
   nothing tying it to pix. **Shipping the formula without fixing this first is the
   most damaging outcome available.**
2. **`pix uninstall` lies.** `resolveBinPaths` in `reset.go` hardcodes
   `~/.local/bin`, so on a brew install it resets all state, removes no binaries,
   reports success, and leaves `pix` on PATH.
3. **Duplicate installs are silent.** `~/.local/bin` usually precedes the brew
   prefix on PATH, so an existing curl user who runs `brew install` keeps running
   the old binary, `brew upgrade`s, and nothing changes. This hits nearly every
   migrating user.
4. **A wrong CI comment.** publish.yml claims `pix-host` has no `main.version`
   symbol so the ldflag is ignored. It does have one (`services/host/main.go:46`)
   and it is stamped. Harmless today, lethal in any second build recipe, because
   `findHostBinary()` (`main.go:272`) refuses to run on a version mismatch.

## The constraint that shapes the packaging

`findHostBinary()` execs `pix-host version` and refuses to run unless it exactly
equals the launcher's stamped version. Two-binary atomicity is not a convention
install.sh chose; it is enforced on every invocation. So any packaging that can
move one binary without the other is a hard outage, which forces:

- One formula owning both binaries. Never two formulae, because Homebrew has no
  cross-formula transaction.
- One tarball per arch containing both. Two `url`s or a `resource` block would mean
  per-arch times per-binary sha256 pairs, any one of which can be wrong.
- A `test do` block asserting both versions match. That is the fitness function for
  the stamping invariant.

## Uninstall ordering

`brew uninstall pix` removes binaries and nothing else, and formulae have **no
uninstall hook** (`uninstall_preflight` is cask-only). A launchd-managed `pix-host
serve` whose binary just vanished will respawn-fail forever, and the only tool that
stops it correctly is the binary just deleted.

So the order is documented everywhere a user might read it at the decision point,
including formula `caveats`: **`pix state uninstall` first, `brew uninstall`
second.**

## Confidence limit

This was planned in a Linux sandbox with no macOS, brew, or Gatekeeper access.
Every macOS-specific claim here is sourced reasoning, not observation: quarantine
behavior, Go's ad-hoc arm64 signing, keg symlink layout, and `os.Executable()` not
resolving symlinks on darwin. The prototype sequence at the top of the build plan
exists to convert those into observations before anything publishes.

## Corrections to onboarding-v3

Section 3 is wrong in two places and the build plan fixes both: the tap is
`mcavage/tap`, not `docker/tap` (pix is not a Docker product), and the formula does
not own "ordinary package dependencies" for the reasons above.
