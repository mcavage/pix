package slackoauth

// RequiredUserScopes is the EXACT read-only Slack user_scope set every
// consumer of this package's rotating credential must request and hold:
// `pix slack setup`'s PKCE authorize URL (cmd/pix/slack_oauth.go) and the
// host slack.go runtime credential source's Client.RequiredScopes (see
// docs/design/slack-setup.md's scope table) both reference THIS list rather
// than each maintaining their own copy, so the two can never drift out of
// sync with each other.
var RequiredUserScopes = []string{
	"search:read",
	"channels:read", "channels:history",
	"groups:read", "groups:history",
	"im:read", "im:history",
	"mpim:read", "mpim:history",
	"users:read", "users:read.email",
}
