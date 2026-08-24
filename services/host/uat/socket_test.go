//go:build unix

package uat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustSessionDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "session")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateSocketDirAcceptsHardenedDir(t *testing.T) {
	dir := mustSessionDir(t)
	if err := ValidateSocketDir(dir); err != nil {
		t.Fatalf("ValidateSocketDir(%s) = %v, want nil", dir, err)
	}
}

func TestValidateSocketDirRefusesSymlink(t *testing.T) {
	real := mustSessionDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketDir(link); err == nil {
		t.Fatal("expected error for symlinked session directory, got nil")
	}
}

func TestValidateSocketDirRefusesLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketDir(dir); err == nil {
		t.Fatal("expected error for 0755 session directory, got nil")
	}
}

func TestValidateSocketDirRefusesRelativePath(t *testing.T) {
	if err := ValidateSocketDir("relative/dir"); err == nil {
		t.Fatal("expected error for relative session directory, got nil")
	}
}

func TestValidateSocketPathRefusesSymlinkedSocket(t *testing.T) {
	dir := mustSessionDir(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.sock")
	sockPath := filepath.Join(dir, SocketFileName)
	if err := os.Symlink(elsewhere, sockPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketPath(sockPath); err == nil {
		t.Fatal("expected error for symlinked socket path, got nil")
	}
}

func TestValidateSocketPathAllowsMissingFile(t *testing.T) {
	dir := mustSessionDir(t)
	sockPath := SessionSocketPath(dir)
	if err := ValidateSocketPath(sockPath); err != nil {
		t.Fatalf("ValidateSocketPath on not-yet-created socket = %v, want nil", err)
	}
}

func TestDialSocketMissingIsBoundedAndActionable(t *testing.T) {
	dir := mustSessionDir(t)
	sockPath := SessionSocketPath(dir)

	start := time.Now()
	_, err := DialSocket(sockPath, 3, 10*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error dialing a socket with no worker listening, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DialSocket took %v, want a bounded retry (attempts*delay), not an unbounded hang", elapsed)
	}
	if got := err.Error(); !strings.Contains(got, "pix run --dev") {
		t.Fatalf("DialSocket error = %q, want it to name the recovery command `pix run --dev`", got)
	}
}

func TestListenThenDialRoundTrips(t *testing.T) {
	dir := mustSessionDir(t)
	sockPath := SessionSocketPath(dir)

	l, err := ListenSocket(sockPath)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer l.Close()

	acceptErr := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := conn.Read(buf); err != nil {
			acceptErr <- err
			return
		}
		if string(buf) != "hello" {
			acceptErr <- os.ErrInvalid
			return
		}
		acceptErr <- nil
	}()

	conn, err := DialSocket(sockPath, 5, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-acceptErr; err != nil {
		t.Fatalf("accept side: %v", err)
	}
}

func TestListenSocketRefusesLiveListener(t *testing.T) {
	dir := mustSessionDir(t)
	sockPath := SessionSocketPath(dir)

	l, err := ListenSocket(sockPath)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer l.Close()

	if _, err := ListenSocket(sockPath); err == nil {
		t.Fatal("expected error listening on a socket path with an active listener, got nil")
	}
}
