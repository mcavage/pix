package launch

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
	"pix/host/launcher"
	"strings"
)

// KitRefSource explains which rule produced the ref, for the one-line notice
// `pix run` prints when it is NOT simply using this build's own version.
type KitRefSource int

const (
	KitRefStamped    KitRefSource = iota // this binary's own version (default / every fallback)
	KitRefFlag                           // --kit-ref
	KitRefConfigPin                      // version_pin in config.toml
	KitRefUnreleased                     // dev/local build: main or a local checkout
)

// ResolveKitRef applies the precedence chain and reports which rule won.
//
// flagRef and pinRef are taken as given (a user who names a ref means it, so
// they are not validated against isReleased, which preserves the escape hatch
// for a branch or unpublished tag.
func ResolveKitRef(version, flagRef, pinRef string) (ref string, src KitRefSource) {
	switch {
	case flagRef != "":
		return flagRef, KitRefFlag
	case pinRef != "":
		return normalizeKitRef(pinRef), KitRefConfigPin
	case !launcher.IsReleased(version):
		return "", KitRefUnreleased
	default:
		return "", KitRefStamped
	}
}

// normalizeKitRef lets a config pin be written either way — version_pin =
// "0.1.0" and version_pin = "v0.1.0" both mean the v0.1.0 tag. A non-semver
// value (a branch name, a sha) passes through untouched.
func normalizeKitRef(pin string) string {
	pin = strings.TrimSpace(pin)
	if launcher.IsReleased(pin) {
		return "v" + pin
	}
	return pin
}

// KitRefNotice is the one line `pix run` prints when the kit is NOT this
// build's own version, so the resolution is never invisible. Returns "" when
// there is nothing worth saying.
func KitRefNotice(version, ref string, src KitRefSource) string {
	switch src {
	case KitRefConfigPin:
		return fmt.Sprintf("pix: kit %s (pinned by version_pin in config.toml)", ref)
	default:
		// --kit-ref is the user's own explicit argument; echoing it back is noise.
		return ""
	}
}
