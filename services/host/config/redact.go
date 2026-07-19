package config

import "strings"

// RedactURL masks embedded credentials in a git/https URL so a display or stored
// line never leaks one. It redacts BOTH userinfo (user:token@host) AND
// credential-bearing query parameters (?access_token=..., ?private_token=...,
// GitHub/GitLab/Bitbucket style). Pure string surgery (no net/url re-encoding) so
// the output format stays stable (`***@host`, param `=***`). Shared by the
// launcher (knowledge remote display) and the host binary (backup manifest notes)
// so both redact identically. A URL with no scheme is returned unchanged.
func RedactURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	scheme := u[:i+3]
	rest := u[i+3:]

	// Split off the authority (up to the first '/', '?', or '#') so an '@' in a
	// path or query is never mistaken for a userinfo separator.
	authEnd := len(rest)
	for j, c := range rest {
		if c == '/' || c == '?' || c == '#' {
			authEnd = j
			break
		}
	}
	authority, tail := rest[:authEnd], rest[authEnd:]
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		authority = "***@" + authority[at+1:]
	}

	// Redact credential-bearing query parameters in the tail (?k=v&...).
	if q := strings.IndexByte(tail, '?'); q >= 0 {
		head := tail[:q+1]
		query := tail[q+1:]
		frag := ""
		if f := strings.IndexByte(query, '#'); f >= 0 {
			query, frag = query[:f], query[f:]
		}
		parts := strings.Split(query, "&")
		for k, kv := range parts {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			if looksSecretParam(kv[:eq]) {
				parts[k] = kv[:eq] + "=***"
			}
		}
		tail = head + strings.Join(parts, "&") + frag
	}

	return scheme + authority + tail
}

// looksSecretParam reports whether a query-parameter name is a known credential
// carrier in git/API remote URLs.
func looksSecretParam(k string) bool {
	lk := strings.ToLower(k)
	switch lk {
	case "access_token", "private_token", "token", "oauth_token",
		"api_key", "apikey", "key", "password", "secret", "auth":
		return true
	}
	return strings.Contains(lk, "token") || strings.Contains(lk, "secret") ||
		strings.Contains(lk, "password") || strings.Contains(lk, "apikey")
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
