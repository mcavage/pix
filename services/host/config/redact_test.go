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
