package main

// upgrade.go — `pix upgrade`: replace the installed launcher binaries.
//
// `pix run` tracks the latest stable KIT and IMAGE on its own (kitref.go), so
// most fixes reach you without touching the binaries. The two host binaries are
// the part that cannot update themselves, and until now the only way to move
// them was to remember the curl|sh line from the README.
//
// This deliberately does NOT reimplement the install. It downloads install.sh
// and runs it, because that script already does the security-relevant work:
// per-asset sha256 verification against a published SHA256SUMS, and a
// verify-EVERYTHING-then-install-everything staging order so a failed download
// can never leave a new pix next to a stale pix-host. Duplicating that in Go
// would mean two implementations of a checksum gate, drifting apart.
//
// Two details that differ from the README's curl|sh:
//
//   - install.sh is pinned to the TAG being installed, not main. An upgrade to
//     v0.1.4 runs v0.1.4's installer, so the installer and the assets it fetches
//     always come from one commit.
//   - PIX_PREFIX defaults to the directory the RUNNING pix lives in, not
//     ~/.local/bin. Someone who installed to /usr/local/bin gets that upgraded,
//     rather than a second copy appearing somewhere else on their PATH.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// upgradeInstallerURL is install.sh at a specific release tag.
func upgradeInstallerURL(version string) string {
	return "https://raw.githubusercontent.com/mcavage/pix/v" + version + "/install.sh"
}

// upgradeOpts is the parsed `pix upgrade` command line.
type upgradeOpts struct {
	Check   bool   // --check: report only, change nothing
	Force   bool   // --force: upgrade even from a local/dev build, or reinstall the same version
	Version string // --version V: install V instead of the latest stable
}

func parseUpgradeArgs(argv []string) (upgradeOpts, error) {
	var o upgradeOpts
	// Same --flag=value / --flag value contract as parseRunArgs (whose valueOf is
	// a closure over its own pre-`--` slice, so it cannot be shared).
	valueOf := func(a string, i *int) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(argv) {
			return "", fmt.Errorf("flag %s needs a value", a)
		}
		*i++
		return argv[*i], nil
	}
	// Tracked separately from o.Version being non-empty: `--version ""` must be a
	// usage error, not a silent fall-through to "install latest".
	versionSet := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch {
		case a == "--check":
			o.Check = true
		case a == "--force":
			o.Force = true
		case name == "--version":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Version = strings.TrimPrefix(strings.TrimSpace(v), "v")
			versionSet = true
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	if versionSet && !isReleased(o.Version) {
		return o, fmt.Errorf("--version %q is not a released semver like 0.1.4", o.Version)
	}
	return o, nil
}

// installPrefix is the directory to install into: wherever the running binary
// lives, so an upgrade replaces THIS pix rather than planting a second one.
// Falls back to install.sh's own default ("") when nothing can be determined,
// which is the same answer as a fresh install.
//
// It must return the path SPELLED THE WAY $PATH spells it, not the realpath.
// install.sh gates on `command -v pix` = $PIX_PREFIX/pix, both before (a
// collision preflight) and after (a resolution assert), and `command -v` does
// not resolve symlinks. Handing it a realpath makes those compare unequal the
// moment any ancestor is a link — /tmp -> /private/tmp on macOS, a symlinked
// $HOME on Linux — and the upgrade dies with a bogus "existing pix command at
// <the very path we are installing to>". So ask PATH the same question the
// script will, and only fall back to the executable's own directory when the
// PATH answer is a DIFFERENT binary (a genuine collision, which install.sh
// should be the one to report).
func installPrefix() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if onPath, lerr := exec.LookPath("pix"); lerr == nil && sameFile(onPath, exe) {
		return filepath.Dir(onPath)
	}
	return filepath.Dir(exe)
}

// sameFile reports whether two paths name the same file on disk, through any
// symlinks. os.SameFile compares inode identity, so it is immune to the
// spelling differences that motivated it.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// upgradePlan is the decision, split out so it is testable without a network or
// a filesystem: given where we are and what is published, what should happen?
type upgradePlan struct {
	Target  string // version to install ("" when there is nothing to do)
	Message string // what to tell the user
	Err     error  // a refusal
}

// planUpgrade decides. current is this build's stamped version, latest is the
// newest published release ("" if it could not be resolved).
func planUpgrade(current, latest string, o upgradeOpts) upgradePlan {
	target := o.Version
	if target == "" {
		if latest == "" {
			return upgradePlan{Err: fmt.Errorf("could not resolve the latest release (offline?); retry, or name one: pix upgrade --version X.Y.Z")}
		}
		target = latest
	}

	// A local/dev build is someone's own `make install`. Silently replacing it
	// with a release would throw away the thing they are testing, and the loss is
	// invisible until they wonder why their change stopped taking effect.
	if !isReleased(current) && !o.Force {
		return upgradePlan{Err: fmt.Errorf("this is a local build (%s), not a release — `pix upgrade` would replace it with %s.\nRebuild from your checkout with `make install`, or force it: pix upgrade --force", current, target)}
	}

	if target == current && !o.Force {
		return upgradePlan{Message: fmt.Sprintf("pix %s is already the latest release.", current)}
	}

	verb := "Upgrading"
	if isReleased(current) && isReleased(target) && olderVersion(target, current) {
		verb = "DOWNGRADING"
	}
	return upgradePlan{Target: target, Message: fmt.Sprintf("%s pix %s -> %s", verb, current, target)}
}

// olderVersion reports whether a < b, both clean released semvers. Used only to
// label a downgrade in the output, so an unparseable component just sorts equal
// rather than erroring — mislabelling a line is not worth failing a command.
func olderVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := atoiSafe(as[i]), atoiSafe(bs[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// runUpgrade is the `pix upgrade` entry point.
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

	// An explicit `pix upgrade` always asks GitHub directly: the 24h memo exists
	// so `pix run` is not a network call, but someone typing "upgrade" wants
	// today's answer, not yesterday's. Refresh the memo while we are here so the
	// next `pix run` benefits from the lookup we just paid for.
	latest := o.Version
	if latest == "" {
		latest = fetchLatestRelease(&http.Client{Timeout: upgradeLookupTimeout}, releasesLatestURL)
		if latest != "" {
			writeLatestReleaseCache(latestReleaseCachePath(), latest, time.Now())
		}
	}

	plan := planUpgrade(version, latest, o)
	if plan.Err != nil {
		fmt.Fprintf(os.Stderr, "pix upgrade: %v\n", plan.Err)
		os.Exit(1)
	}

	if o.Check {
		if plan.Target == "" {
			fmt.Println(plan.Message)
			return
		}
		fmt.Printf("pix %s is installed; %s is available.\n  upgrade:  pix upgrade\n", version, plan.Target)
		return
	}
	if plan.Target == "" {
		fmt.Println(plan.Message)
		return
	}

	fmt.Println(plan.Message)
	if err := execInstaller(plan.Target, installPrefix(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "pix upgrade: %v\n", err)
		fmt.Fprintf(os.Stderr, "Nothing was replaced. Install by hand:\n  curl -fsSL %s | sh\n", upgradeInstallerURL(plan.Target))
		os.Exit(1)
	}
}

// upgradeLookupTimeout is longer than the one `pix run` uses: an explicit
// upgrade can afford to wait for a slow network, where a run cannot.
const upgradeLookupTimeout = 10 * time.Second

// execInstaller downloads install.sh for the target release and runs it. The
// script does the download, the sha256 verification, and the staged install; it
// also no-ops when the bytes on disk already match, which makes a repeated
// `pix upgrade` harmless.
func execInstaller(target, prefix string, stdout, stderr io.Writer) error {
	return execInstallerFrom(upgradeInstallerURL(target), target, prefix, stdout, stderr)
}

// execInstallerFrom is execInstaller with the URL injected, so the tests can
// drive the whole fetch-stage-run path against a local server instead of GitHub.
func execInstallerFrom(url, target, prefix string, stdout, stderr io.Writer) error {
	resp, err := (&http.Client{Timeout: upgradeLookupTimeout}).Get(url)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: HTTP %d (is v%s published?)", url, resp.StatusCode, target)
	}
	// Cap the read: this is a shell script we are about to execute, so a
	// surprise multi-gigabyte body should fail rather than fill the disk.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading %s: %w", url, err)
	}

	f, err := os.CreateTemp("", "pix-install-*.sh")
	if err != nil {
		return fmt.Errorf("staging the installer: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("staging the installer: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("staging the installer: %w", err)
	}

	// PIX_VERSION pins the assets to the release we decided on, so the installer
	// never re-resolves "latest" and lands somewhere other than what we printed.
	cmd := exec.Command("sh", name)
	cmd.Env = append(os.Environ(), "PIX_VERSION="+target)
	if prefix != "" {
		cmd.Env = append(cmd.Env, "PIX_PREFIX="+prefix)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin // install.sh prompts on a binary-collision preflight
	return cmd.Run()
}

const upgradeUsage = `usage: pix upgrade [--check] [--version X.Y.Z] [--force]

Replace the installed pix + pix-host binaries with a published release.

` + "`pix run`" + ` already tracks the latest stable KIT and IMAGE by itself, so most
fixes reach you without upgrading. This is for the two host binaries, which
cannot update themselves.

flags:
  --check          report what is installed vs published; change nothing
  --version X.Y.Z  install this exact release instead of the latest stable
                   (also how you downgrade)
  --force          upgrade from a local/dev build, or reinstall the same version

Under the hood it runs the official install.sh for the target release, which
verifies every binary's sha256 against the published SHA256SUMS and installs
only once ALL of them verify. It installs into the directory the running pix
lives in (override with PIX_PREFIX), and does nothing when the bytes already
match, so running it twice is harmless.
`
