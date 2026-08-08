package mcp

import "pix/host/config"

// contains/match are this package's copy of a two-line subsequence check the
// cmd/pix tests also have. Sharing it would mean exporting a test helper from
// a package that has no other reason to know about mcp; a copy this small is
// the cheaper honesty.

// contains reports whether the ordered args slice contains the given
// consecutive subsequence.
func contains(args, sub []string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(args); i++ {
		if match(args[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func match(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// defaultCfg is a minimal Config with the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}}
}
