package main

// kitref.go — which release does `pix run` actually run?
//
// This used to resolve the latest stable release at run time, with a 24-hour
// cache and silent fallback. That existed because a launcher installed once
// could otherwise remain pinned to a broken release indefinitely. Homebrew is
// now the user-initiated update channel, so the kit and image move only when
// the launcher moves. This also prevents kit metadata from changing without an
// explicit upgrade.
//
// Precedence is: --kit-ref, version_pin, then the launcher's stamped version.

import (
	"fmt"
	"strings"
)

// kitRefSource explains which rule produced the ref, for the one-line notice
// `pix run` prints when it is NOT simply using this build's own version.
type kitRefSource int

const (
	kitRefStamped    kitRefSource = iota // this binary's own version (default / every fallback)
	kitRefFlag                           // --kit-ref
	kitRefConfigPin                      // version_pin in config.toml
	kitRefUnreleased                     // dev/local build: main or a local checkout
)

// resolveKitRef applies the precedence chain and reports which rule won.
//
// flagRef and pinRef are taken as given (a user who names a ref means it, so
// they are not validated against isReleased, which preserves the escape hatch
// for a branch or unpublished tag.
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
	case kitRefConfigPin:
		return fmt.Sprintf("pix: kit %s (pinned by version_pin in config.toml)", ref)
	default:
		// --kit-ref is the user's own explicit argument; echoing it back is noise.
		return ""
	}
}
