package main

import (
	"strings"
	"testing"
)

func TestContainsSecretShapeMatchesKnownShapes(t *testing.T) {
	cases := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----",
		"my key is AKIAABCDEFGHIJKLMNOP thanks",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"xoxb-1234567890-abcdefghijklmnop",
		"sk-abcdefghijklmnopqrstuvwx0123456789",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dQw4w9WgXcQ_dGmC1EBmDzYRWDRhAAAAAAAAAAA",
		"api_key: sk_live_51H8xyzabcdefghijklmno",
		"password=SuperSecretValue123",
		"here's the key sk_live_51H8xyzabcdefghijklmno for the demo account",
		"the google key is AIzaabcdefghijklmnopqrstuvwxyz012345678 for the demo project",
		// Realistic env-var shapes: the keyword is glued to a longer
		// SCREAMING_SNAKE_CASE name by underscores on BOTH sides, which a plain
		// \b anchor misses (underscore is a word character).
		"AWS_SECRET_ACCESS_KEY=abcdefghijklmnopqrstuvwx",
		"export SLACK_BOT_TOKEN=xoxb-1234567890-abcdefghijklmnop",
		"DATABASE_PASSWORD=SuperSecretValue123",
	}
	for _, c := range cases {
		if !containsSecretShape(c) {
			t.Errorf("containsSecretShape(%q) = false, want true", c)
		}
	}
}

// TestContainsSecretShapeAvoidsFalsePositives covers ordinary text (including
// a NAMED but un-assigned env var) and the realistic SHA/digest regression:
// an all-hex run must not be flagged just because it's long and unbroken.
func TestContainsSecretShapeAvoidsFalsePositives(t *testing.T) {
	cases := []string{
		"the user prefers tabs over spaces",
		"the project is called pix-memory-capture-modes",
		"the AWS_SECRET_ACCESS_KEY env var controls which credential is used",
		"the regression was introduced in commit 9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a",
		"the image digest is sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e", // 48 hex chars, no prefix
		strings.Repeat("deadbeef", 5),                      // all-hex, well over 32 chars
	}
	for _, c := range cases {
		if containsSecretShape(c) {
			t.Errorf("containsSecretShape(%q) = true, want false (false positive)", c)
		}
	}
}

// TestContainsSecretShapeHighEntropyRunWithNoKnownPrefix: a long unbroken run
// mixing non-hex letters and digits is still flagged with no vendor prefix
// or label -- what distinguishes it from the all-hex SHA/digest case above.
func TestContainsSecretShapeHighEntropyRunWithNoKnownPrefix(t *testing.T) {
	if !containsSecretShape("here you go: 9fXa7bYc5dZe3f2a1bWc9dQe7f6aRb4cGd2eHf0aJb8cKd6e") {
		t.Error("a long unbroken alnum run with non-hex letters should be flagged even with no known prefix")
	}
}

// TestMemCaptureSecretFilterStages proves both filter points: a secret-shaped
// USER MESSAGE never reaches the watcher (stage 1), and a secret-shaped
// EXTRACTED item is dropped before storage even when the input looked clean
// (stage 2), while a co-extracted clean item survives.
func TestMemCaptureSecretFilterStages(t *testing.T) {
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")

	st1 := watchServer(t, `{"facts":["should never be seen"],"corrections":[]}`)
	memCaptureSem <- struct{}{}
	memCapture(st1, "here is my key AKIAABCDEFGHIJKLMNOP please remember it", "", false, "default")
	if hits, err := st1.recall("*", 100, 1000000, "", "", "default"); err != nil {
		t.Fatal(err)
	} else if len(hits) != 0 {
		t.Fatalf("stage 1: a secret-shaped input must block capture entirely, got %+v", hits)
	}

	content := `{"facts":["the user's token is ghp_abcdefghijklmnopqrstuvwxyz0123456789","the user prefers dark mode"],"corrections":[]}`
	st2 := watchServer(t, content)
	memCaptureSem <- struct{}{}
	memCapture(st2, "an assertion-bearing message, not a question.", "", false, "default")
	hits, err := st2.recall("*", 100, 1000000, "", "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].content != "the user prefers dark mode" {
		t.Fatalf("stage 2: expected only the clean fact to survive, got %+v", hits)
	}
}
