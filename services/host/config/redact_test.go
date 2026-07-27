package config

import "testing"

func TestRedactURL_Userinfo(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/a/b.git": "https://***@github.com/a/b.git",
		"https://github.com/a/b.git":          "https://github.com/a/b.git",
		"git@github.com:a/b.git":              "git@github.com:a/b.git", // no scheme, untouched
		"ssh://git@host/a/b":                  "ssh://***@host/a/b",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLooksSecretShapedDoesNotFlagSlackPublicIDs: a Slack OAuth app's public
// client_id and the 1Password vault/document ids that locate (but never
// contain) the rotating credential blob must never be misjudged as
// secret-shaped — they are public wiring, not tokens, and no code should ever
// hide them from a `config show`/`doctor` listing.
func TestLooksSecretShapedDoesNotFlagSlackPublicIDs(t *testing.T) {
	cases := map[string]string{
		"client_id":         "1234567890.1234567890123",
		"oauth_vault_id":    "Private",
		"oauth_document_id": "item123",
	}
	for key, val := range cases {
		if LooksSecretShaped(key, val) {
			t.Errorf("LooksSecretShaped(%q, %q) = true, want false (public id, not a secret)", key, val)
		}
	}
}

// TestRedactURL_QueryToken is the regression: a credential in the query string
// (no userinfo '@') must be masked, not passed through verbatim.
func TestRedactURL_QueryToken(t *testing.T) {
	cases := map[string]string{
		"https://host/repo.git?access_token=SECRET":     "https://host/repo.git?access_token=***",
		"https://host/r.git?private_token=abc&ref=main": "https://host/r.git?private_token=***&ref=main",
		"https://host/r.git?ref=main&api_key=zzz":       "https://host/r.git?ref=main&api_key=***",
		"https://u:p@host/r.git?token=T#frag":           "https://***@host/r.git?token=***#frag",
		"https://host/r.git?ref=main":                   "https://host/r.git?ref=main", // nothing secret
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
