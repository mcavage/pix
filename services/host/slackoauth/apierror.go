package slackoauth

import (
	"errors"
	"fmt"
)

// Sentinel errors for the specific Slack Web API error codes that mean a
// rotating OAuth credential is DEFINITIVELY dead: nothing about it can be
// repaired by refreshing again or retrying a revoke, and there is no path
// back but a full re-authorization (a fresh Client.Exchange). Callers MUST
// test for these with errors.Is (via IsDeadCredential below), never by
// matching Slack's raw "error" string with strings.Contains — a brittle
// pattern this package's own error classification (ClassifyAPIError) exists
// specifically to make unnecessary in production code.
var (
	// ErrInvalidAuth is Slack's invalid_auth: the bearer token presented was
	// rejected outright (commonly because it was already revoked, by hand
	// or elsewhere).
	ErrInvalidAuth = errors.New("slackoauth: slack rejected the token (invalid_auth)")
	// ErrTokenRevoked is Slack's token_revoked: the token existed and was
	// explicitly revoked (matches auth.revoke's own confirmation semantics,
	// just reported back the other direction — by a LATER call discovering
	// the earlier revocation).
	ErrTokenRevoked = errors.New("slackoauth: slack reports the token was already revoked (token_revoked)")
	// ErrInvalidRefreshToken is Slack's invalid_refresh_token: the rotating
	// refresh_token this Manager holds no longer works — most commonly
	// because the grant was revoked, or the refresh token was already
	// consumed by a rotation this store never saw.
	ErrInvalidRefreshToken = errors.New("slackoauth: slack rejected the refresh token (invalid_refresh_token)")
)

// ClassifyAPIError turns a Slack Web API "error" response code into a typed
// error: one of the three sentinels above when code is one Slack uses to
// mean the credential is already dead, wrapped (%w) so errors.Is still
// matches it through prefix; any other code falls back to a plain error
// carrying prefix and the raw code, exactly as an unclassified failure
// always has. Every production caller that classifies a Slack API error
// response (the oauth.v2.access exchange/refresh, and auth.revoke) MUST
// route through this rather than hand-rolling its own string match, so the
// set of "this credential is dead" codes lives in exactly one place.
func ClassifyAPIError(prefix, code string) error {
	switch code {
	case "invalid_auth":
		return fmt.Errorf("%s: %w", prefix, ErrInvalidAuth)
	case "token_revoked":
		return fmt.Errorf("%s: %w", prefix, ErrTokenRevoked)
	case "invalid_refresh_token":
		return fmt.Errorf("%s: %w", prefix, ErrInvalidRefreshToken)
	default:
		return fmt.Errorf("%s: %s", prefix, code)
	}
}

// IsDeadCredential reports whether err means the rotating OAuth credential a
// Manager/Store is responsible for is definitively unusable: the 30-day
// grant window has passed (ErrGrantExpired), or Slack itself said so via one
// of invalid_auth/token_revoked/invalid_refresh_token (ClassifyAPIError).
// Nothing else is ever treated this way — a network failure, a 1Password
// I/O error, or any other Slack API error code must still abort the caller
// rather than be silently treated as "already gone". A caller like `pix
// slack disable` uses this to decide whether a failure to obtain/revoke a
// live token means there is simply nothing left to revoke (safe to continue
// local cleanup) or a real, retryable problem (must abort with nothing
// removed).
func IsDeadCredential(err error) bool {
	return errors.Is(err, ErrGrantExpired) ||
		errors.Is(err, ErrInvalidAuth) ||
		errors.Is(err, ErrTokenRevoked) ||
		errors.Is(err, ErrInvalidRefreshToken)
}
