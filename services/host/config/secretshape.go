package config

import "strings"

// LooksSecretShaped reports whether a NON-ref, non-allowlisted op-refs.env value
// looks like a pasted secret: a Slack token (xox* prefix), a literal for the
// known-secret GOG_KEYRING_PASSWORD key, or a high-entropy token (long, no
// whitespace, mixed letters+digits). It errs toward not-flagging benign values;
// the caller NEVER prints the value regardless. It lives here, beside the
// op-refs template and its allowlist, so every judge of "is this a pasted
// secret" answers identically.
func LooksSecretShaped(key, val string) bool {
	if val == "" {
		return false
	}
	if strings.HasPrefix(val, "xox") || key == "GOG_KEYRING_PASSWORD" {
		return true
	}
	if len(val) < 20 || strings.ContainsAny(val, " \t") {
		return false
	}
	var hasLetter, hasDigit bool
	for _, c := range val {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			hasLetter = true
		}
	}
	return hasLetter && hasDigit
}
