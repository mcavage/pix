package main

import (
	"regexp"
	"strings"
)

// doctor_classify.go is the PURE probe-outcome classifier: given the captured
// output (+ error) of a failed readiness probe, decide whether the failure is
// a positive policy/permission DENIAL (verdict denied), a missing/expired
// credential the user can fix (an auth TODO), or merely UNVERIFIABLE
// (transport/infra/generic failure — doctor does not know, so it must not
// claim a verified failure).
//
// The matching is deliberately conservative:
//   - only EXPLICIT denial tokens count as denied: "forbidden",
//     "access_denied"/"access denied", a denial verb bound to "policy"
//     ("not allowed by policy", "denied by org policy", "policy forbids", an
//     explicit "policy denial"), "permission denied" IN a policy context, or
//     an HTTP 403 whose body carries a denial token. A bare "policy" word
//     (help text, flag names, "see the retry policy docs") NEVER matches.
//   - a bare 401/unauthorized is an AUTH gap for the caller to surface as a
//     credential TODO — it is NOT an org-policy denial.
//   - everything else — timeout, connection refused, EOF, DNS, TLS, a generic
//     non-zero exit — is unverifiable.

// probeClass is the classified outcome of a failed probe.
type probeClass int

const (
	// probeUnverifiable: the probe could not prove anything (transport, DNS,
	// TLS, timeout, generic failure). Callers map this to readiness.VerdictUnverifiable.
	probeUnverifiable probeClass = iota
	// probeDenied: the remote side POSITIVELY refused by policy/permission.
	// Callers map this to readiness.VerdictDenied.
	probeDenied
	// probeAuthTodo: authentication is missing/expired (bare 401 /
	// unauthorized). An actionable credential gap for the caller to surface
	// as a verified todo — NOT an org-policy denial.
	probeAuthTodo
)

// String returns the machine-readable evidence token for the class.
func (p probeClass) String() string {
	switch p {
	case probeDenied:
		return "denied"
	case probeAuthTodo:
		return "auth-todo"
	default:
		return "unverifiable"
	}
}

// deniedPatterns match ONLY explicit policy/permission denials. Every pattern
// that involves the word "policy" binds it to a denial verb, so incidental
// mentions of "policy" (help text, `--policy` flags, documentation pointers)
// can never classify as denied.
var deniedPatterns = []*regexp.Regexp{
	// The universal HTTP/API denial words.
	regexp.MustCompile(`\bforbidden\b`),
	regexp.MustCompile(`\baccess[_ ]denied\b`),
	// A denial verb bound to "policy" within a few words:
	// "not allowed by policy", "denied by org policy", "blocked by your
	// organization's policy", "rejected by workspace policies", ...
	regexp.MustCompile(`\b(?:denied|not allowed|not permitted|blocked|prohibited|rejected|forbidden)\s+by\s+(?:\S+\s+){0,3}polic(?:y|ies)\b`),
	// "policy" as the acting subject: "policy denies/forbids/prohibits …",
	// or the noun form "policy denial" (covers "explicit policy denial").
	regexp.MustCompile(`\bpolic(?:y|ies)\s+(?:denial|denied|denies|forbids|forbade|prohibits|blocks|does not allow)\b`),
	// "permission denied" ONLY in a policy context on the same line — a bare
	// "permission denied" (file perms, ssh) stays unverifiable.
	regexp.MustCompile(`\bpermission denied\b[^\n.]*\bpolic(?:y|ies)\b`),
}

// deniedBodyTokens are the denial words that, combined with an HTTP 403 status
// in the same output, make the 403 a positive denial. A bare 403 with no
// denial body stays unverifiable (proxies and gateways emit contentless 403s
// for transport reasons).
var deniedBodyTokens = regexp.MustCompile(`\b(?:denied|denial|forbidden|not allowed|not permitted|prohibited)\b`)

var http403 = regexp.MustCompile(`\b403\b`)

// authPatterns match a missing/expired credential: a bare 401 / unauthorized.
var authPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b401\b`),
	regexp.MustCompile(`\bunauthorized\b`),
	regexp.MustCompile(`\bunauthenticated\b`),
	regexp.MustCompile(`\bauthentication required\b`),
}

// classifyProbeFailure classifies a FAILED probe's combined output + error.
// It is pure (no I/O) and case-insensitive. Explicit denial tokens win over
// auth tokens (a "403 forbidden" that also mentions a token is still a
// denial); everything unmatched is unverifiable — the safe default that never
// invents a verified failure out of a flaky transport.
func classifyProbeFailure(output string, err error) probeClass {
	hay := strings.ToLower(output)
	if err != nil {
		hay += "\n" + strings.ToLower(err.Error())
	}
	for _, re := range deniedPatterns {
		if re.MatchString(hay) {
			return probeDenied
		}
	}
	if http403.MatchString(hay) && deniedBodyTokens.MatchString(hay) {
		return probeDenied
	}
	for _, re := range authPatterns {
		if re.MatchString(hay) {
			return probeAuthTodo
		}
	}
	return probeUnverifiable
}
