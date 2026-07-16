package config

import "strings"

// RedactURL masks any userinfo (user:token@) in a git/https URL so a display or
// stored line never leaks an embedded credential. It is shared by the launcher
// (knowledge remote display) and the host binary (backup manifest notes) so both
// redact identically. A URL with no scheme or no userinfo is returned unchanged.
func RedactURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return u
	}
	return u[:i+3] + "***@" + rest[at+1:]
}

// LooksSecretShaped reports whether a NON-ref, non-allowlisted op-refs.env value
// looks like a pasted secret: a Slack token (xox* prefix), a literal for the
// known-secret GOG_KEYRING_PASSWORD key, or a high-entropy token (long, no
// whitespace, mixed letters+digits). It intentionally errs toward not-flagging
// obviously-benign values; the caller NEVER prints the value regardless. Shared
// by doctor (lint) and backup (pre-archive warning) so both judge identically.
func LooksSecretShaped(key, val string) bool {
	if val == "" {
		return false
	}
	if strings.HasPrefix(val, "xox") {
		return true
	}
	if key == "GOG_KEYRING_PASSWORD" {
		return true
	}
	if len(val) >= 20 && !strings.ContainsAny(val, " \t") {
		var hasLetter, hasDigit bool
		for _, c := range val {
			switch {
			case c >= '0' && c <= '9':
				hasDigit = true
			case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
				hasLetter = true
			}
		}
		if hasLetter && hasDigit {
			return true
		}
	}
	return false
}
