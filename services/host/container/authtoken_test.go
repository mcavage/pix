package container

import (
	"os"
	"strings"
	"testing"

	"pix/host/pixhome"
)

// TestEnsureMemoryAuthToken_IdempotentAndReadable: a second call returns the
// SAME token, and ReadMemoryAuthToken (the read-only half) round-trips it —
// an already-created container's baked-in bearer must never desync from a
// freshly registered MCP URL across `pix setup` reruns.
func TestEnsureMemoryAuthToken_IdempotentAndReadable(t *testing.T) {
	home := pixhome.New(t.TempDir())
	tok1, err := EnsureMemoryAuthToken(home)
	if err != nil {
		t.Fatalf("EnsureMemoryAuthToken: %v", err)
	}
	if tok1 == "" {
		t.Fatal("expected a non-empty generated token")
	}
	tok2, err := EnsureMemoryAuthToken(home)
	if err != nil {
		t.Fatalf("EnsureMemoryAuthToken (2nd call): %v", err)
	}
	if tok2 != tok1 {
		t.Fatalf("token changed across calls: %q != %q", tok1, tok2)
	}
	read, err := ReadMemoryAuthToken(home)
	if err != nil || read != tok1 {
		t.Fatalf("ReadMemoryAuthToken = (%q, %v), want (%q, nil)", read, err, tok1)
	}
}

// TestMemoryAuthTokenPath_FileIsRawTokenMode0600: review round 1 blocker
// #1's on-disk shape. The file is bind-mounted READ-ONLY straight into the
// pix-memory container at AuthTokenMountPath, so its on-host content must be
// exactly what pix-memory reads back on the other side of that mount: the
// RAW token, no "MEMORY_AUTH_TOKEN=" env-file wrapping, trimmed of
// whitespace by the reader on both ends. Mode 0600: this is a secret file,
// not a config file.
func TestMemoryAuthTokenPath_FileIsRawTokenMode0600(t *testing.T) {
	home := pixhome.New(t.TempDir())
	tok, err := EnsureMemoryAuthToken(home)
	if err != nil {
		t.Fatalf("EnsureMemoryAuthToken: %v", err)
	}
	path := MemoryAuthTokenPath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(data)); got != tok {
		t.Fatalf("file content = %q, want the raw token %q (no KEY=value wrapping)", got, tok)
	}
	if strings.Contains(string(data), "=") {
		t.Fatalf("file content %q must not carry an env-file KEY=value wrapper", string(data))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestReadMemoryAuthToken_MissingFileIsEmptyNotError: the pre-`pix setup`
// state (or a caller that only ever reads) is "no token yet", never a
// failure — every current builtin-MCP caller already treats an empty
// return as "omit that built-in".
func TestReadMemoryAuthToken_MissingFileIsEmptyNotError(t *testing.T) {
	home := pixhome.New(t.TempDir())
	tok, err := ReadMemoryAuthToken(home)
	if err != nil || tok != "" {
		t.Fatalf("ReadMemoryAuthToken on a fresh home = (%q, %v), want (\"\", nil)", tok, err)
	}
}
