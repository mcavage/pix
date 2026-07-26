package main

// kitref.go — which release does `pix run` actually run?
//
// The launcher used to pin the kit to its OWN stamped version, forever. That
// made the binary and the kit/image it launches move in lockstep, which is a
// real property — they are published from one commit by one CI run. It also
// meant a consumer who installed once was frozen there: every fix published
// afterwards was invisible to them, with nothing in the CLI even hinting that a
// newer release existed. 0.1.1 shipped an image that could not boot at all, and
// an installed launcher would have kept pinning it indefinitely.
//
// So the default is now the LATEST STABLE release, resolved at run time. The
// tag it resolves to is still a concrete immutable version (never a moving
// `:latest` — sbx caches materialized templates per tag name and would serve a
// stale one forever; see the publish workflow's header).
//
// Three rules keep this from becoming a liability:
//
//   1. It is CACHED (24h). A `pix run` is an interactive command; it must not
//      pay a network round trip every invocation.
//   2. It NEVER blocks or fails a run. No network, GitHub down, a corrupt cache,
//      a garbage response — every path falls back to the launcher's stamped
//      version, which is exactly the old behaviour.
//   3. It is OVERRIDABLE, precedence high to low: --kit-ref, `version_pin` in
//      config.toml, the resolved latest stable, the stamped version.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// latestReleaseTTL is how long a resolved "latest stable" is trusted. Long
// enough that a normal day of `pix run` costs one lookup; short enough that a
// fix published this morning is picked up by this afternoon.
const latestReleaseTTL = 24 * time.Hour

// latestReleaseTimeout bounds the lookup. `pix run` is interactive: a slow or
// captive network must cost a beat, not the run.
const latestReleaseTimeout = 2 * time.Second

// releasesLatestURL is GitHub's "latest release" redirect. It answers with a
// 302 to /releases/tag/v<version>, so the version is readable from the Location
// header alone — no API token, no rate-limited API call, no JSON. install.sh
// resolves the version the same way.
const releasesLatestURL = "https://github.com/mcavage/pix/releases/latest"

// latestReleaseCache is the on-disk memo.
type latestReleaseCache struct {
	Version   string    `json:"version"`    // clean semver, no leading v
	CheckedAt time.Time `json:"checked_at"` // when the lookup ran
}

// latestReleaseCachePath is where the memo lives. Under the cache dir, not the
// state dir: losing it costs one HTTP request, so it is the definition of
// regenerable.
func latestReleaseCachePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "pix", "latest-release.json")
}

// readLatestReleaseCache returns the cached version when the memo is present,
// parseable, and still inside the TTL. Every failure mode returns "" — a
// corrupt or half-written cache must be indistinguishable from no cache.
func readLatestReleaseCache(path string, now time.Time) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c latestReleaseCache
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	if !isReleased(c.Version) {
		return ""
	}
	// A CheckedAt in the future (clock skew, a restored backup) is not trusted:
	// treat it as expired rather than pinning that version until the clock
	// catches up.
	age := now.Sub(c.CheckedAt)
	if age < 0 || age > latestReleaseTTL {
		return ""
	}
	return c.Version
}

// writeLatestReleaseCache persists the memo. Best-effort by design: a run must
// never fail because a cache could not be written, it just re-checks next time.
// Written via a temp file + rename so a killed process cannot leave a truncated
// JSON blob that the next read has to defend against.
func writeLatestReleaseCache(path, version string, now time.Time) {
	if path == "" || !isReleased(version) {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	b, err := json.Marshal(latestReleaseCache{Version: version, CheckedAt: now})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".latest-release-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, werr := tmp.Write(b); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return
	}
	if tmp.Close() != nil {
		_ = os.Remove(name)
		return
	}
	if os.Rename(name, path) != nil {
		_ = os.Remove(name)
	}
}

// parseLatestReleaseLocation pulls the version out of a /releases/latest
// redirect target like ".../releases/tag/v0.1.2". Returns "" for anything that
// is not a clean released semver, so a GitHub error page, a login redirect, or
// a future URL shape degrades to "no answer" instead of a bogus pin.
func parseLatestReleaseLocation(loc string) string {
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return ""
	}
	v := strings.TrimSpace(loc[i+len("/tag/"):])
	// Drop a query/fragment if GitHub ever appends one.
	if j := strings.IndexAny(v, "?#"); j >= 0 {
		v = v[:j]
	}
	v = strings.TrimPrefix(v, "v")
	if !isReleased(v) {
		return ""
	}
	return v
}

// fetchLatestRelease asks GitHub for the newest published release. Returns ""
// on ANY failure — this is an optimization, not a dependency.
func fetchLatestRelease(client *http.Client, url string) string {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	// The redirect was followed (default client policy): the final URL carries
	// the tag. If a caller supplied a non-following client, read Location.
	if loc := resp.Request.URL.String(); loc != "" {
		if v := parseLatestReleaseLocation(loc); v != "" {
			return v
		}
	}
	return parseLatestReleaseLocation(resp.Header.Get("Location"))
}

// resolveLatestRelease returns the newest published version, using the cache
// when warm and refreshing it otherwise. "" means "could not determine", which
// every caller must treat as "keep the stamped pin".
func resolveLatestRelease(client *http.Client, now time.Time) string {
	path := latestReleaseCachePath()
	if v := readLatestReleaseCache(path, now); v != "" {
		return v
	}
	v := fetchLatestRelease(client, releasesLatestURL)
	if v == "" {
		return ""
	}
	writeLatestReleaseCache(path, v, now)
	return v
}

// kitRefSource explains which rule produced the ref, for the one-line notice
// `pix run` prints when it is NOT simply using this build's own version.
type kitRefSource int

const (
	kitRefStamped    kitRefSource = iota // this binary's own version (default / every fallback)
	kitRefFlag                           // --kit-ref
	kitRefConfigPin                      // version_pin in config.toml
	kitRefLatest                         // resolved latest stable release
	kitRefUnreleased                     // dev/local build: main or a local checkout
)

// resolveKitRef applies the precedence chain and reports which rule won.
//
// flagRef and pinRef are taken as given (a user who names a ref means it, so
// they are NOT validated against isReleased — this is also the escape hatch for
// pinning a branch or an unpublished tag). latest is consulted only for a
// RELEASED build: an unreleased binary already has its own handling (local
// checkout, else #ref=main) and must not be silently pointed at a release whose
// kit its argv may not match.
func resolveKitRef(version, flagRef, pinRef, latest string) (ref string, src kitRefSource) {
	switch {
	case flagRef != "":
		return flagRef, kitRefFlag
	case pinRef != "":
		return normalizeKitRef(pinRef), kitRefConfigPin
	case !isReleased(version):
		return "", kitRefUnreleased
	case latest != "" && latest != version:
		return "v" + latest, kitRefLatest
	default:
		return "", kitRefStamped
	}
}

// normalizeKitRef lets a config pin be written either way — version_pin =
// "0.1.0" and version_pin = "v0.1.0" both mean the v0.1.0 tag. A non-semver
// value (a branch name, a sha) passes through untouched.
func normalizeKitRef(pin string) string {
	pin = strings.TrimSpace(pin)
	if isReleased(pin) {
		return "v" + pin
	}
	return pin
}

// kitRefNotice is the one line `pix run` prints when the kit is NOT this
// build's own version, so the resolution is never invisible. Returns "" when
// there is nothing worth saying.
func kitRefNotice(version, ref string, src kitRefSource) string {
	switch src {
	case kitRefLatest:
		return fmt.Sprintf("pix: kit %s (latest stable; this launcher is %s — `pix run --kit-ref v%s` to pin it back)", ref, version, version)
	case kitRefConfigPin:
		return fmt.Sprintf("pix: kit %s (pinned by version_pin in config.toml)", ref)
	default:
		// --kit-ref is the user's own explicit argument; echoing it back is noise.
		return ""
	}
}
