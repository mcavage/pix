package launcher

import "regexp"

// releasedVersionRE matches a CLEAN released semver like "0.0.16" — the shape a
// CI release stamps, for which a matching git tag "v0.0.16" is expected to
// exist. Anything else (an unstamped "dev" build, a "0.0.16+local" local build,
// or non-semver) is treated as UNRELEASED, so nothing ever pins a nonexistent
// v<version> tag.
var releasedVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// IsReleased reports whether version is a clean released semver whose git tag
// is expected to exist. It lives beside Version because "which build am I" and
// "is that build a release" are the same question asked twice.
func IsReleased(version string) bool { return releasedVersionRE.MatchString(version) }
