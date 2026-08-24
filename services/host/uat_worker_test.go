package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// recordingServer is a trivial connServer double: it proves uatWorkerLoop's
// accept/serialize/survive-EOF contract without constructing a real UAT
// Runner (workflow/uat's Runner needs a repo, adapters, and a browser
// factory — none of which this transport-level test cares about).
type recordingServer struct {
	calls *int32
	in    io.Reader
	out   io.Writer
}

func (r *recordingServer) Serve(ctx context.Context) error {
	atomic.AddInt32(r.calls, 1)
	// Read to EOF exactly like workflow/uat.MCPServer.Serve does: a client
	// closing its write side (or the whole connection) must end THIS call,
	// not the worker loop.
	_, err := io.Copy(r.out, r.in)
	return err
}

func newRecordingServerFactory(calls *int32) func(io.Reader, io.Writer) connServer {
	return func(in io.Reader, out io.Writer) connServer {
		return &recordingServer{calls: calls, in: in, out: out}
	}
}

func listenUnixForTest(t *testing.T) net.Listener {
	t.Helper()
	dir := t.TempDir()
	l, err := net.Listen("unix", filepath.Join(dir, "worker.sock"))
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	return l
}

func TestUatWorkerLoopSurvivesClientEOF(t *testing.T) {
	l := listenUnixForTest(t)
	addr := l.Addr().String()

	var calls int32
	loopErr := make(chan error, 1)
	go func() { loopErr <- uatWorkerLoop(context.Background(), l, newRecordingServerFactory(&calls)) }()

	// First client: connect, write, then close (client EOF).
	c1, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	if _, err := c1.Write([]byte("hello")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Half-close (CloseWrite), not a full Close: this is the client EOF the
	// worker must survive, without racing the server's own echo-back write
	// against a fully torn-down connection.
	if uc, ok := c1.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			t.Fatalf("close-write 1: %v", err)
		}
	} else if err := c1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	io.ReadAll(c1) //nolint:errcheck // drain the echoed bytes so the server's write does not race a torn-down conn
	_ = c1.Close()

	// Give the loop a moment to process the first connection and return to
	// Accept, then prove the worker is still alive by connecting again.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&calls) < 1 {
		select {
		case <-deadline:
			t.Fatal("worker never served the first connection")
		case <-time.After(5 * time.Millisecond):
		}
	}

	select {
	case err := <-loopErr:
		t.Fatalf("uatWorkerLoop exited after client EOF (must survive it): %v", err)
	default:
	}

	c2, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial 2 (worker should still be accepting): %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}

	deadline = time.After(2 * time.Second)
	for atomic.LoadInt32(&calls) < 2 {
		select {
		case <-deadline:
			t.Fatal("worker never served the second connection; it did not survive the first client's EOF")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// blockingServer holds Serve open until told to finish, so the test can prove
// the loop does not start a second Serve while one is in flight (single-
// flight / serialized clients).
type blockingServer struct {
	started  chan struct{}
	finish   chan struct{}
	sequence *[]int
	id       int
}

func (b *blockingServer) Serve(ctx context.Context) error {
	close(b.started)
	<-b.finish
	*b.sequence = append(*b.sequence, b.id)
	return nil
}

func TestUatWorkerLoopSerializesClients(t *testing.T) {
	l := listenUnixForTest(t)
	addr := l.Addr().String()

	var sequence []int
	first := &blockingServer{started: make(chan struct{}), finish: make(chan struct{}), sequence: &sequence, id: 1}
	second := &blockingServer{started: make(chan struct{}), finish: make(chan struct{}), sequence: &sequence, id: 2}
	served := 0
	factory := func(in io.Reader, out io.Writer) connServer {
		served++
		if served == 1 {
			return first
		}
		return second
	}

	loopErr := make(chan error, 1)
	go func() { loopErr <- uatWorkerLoop(context.Background(), l, factory) }()

	c1, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.Close()

	select {
	case <-first.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first connection never reached Serve")
	}

	c2, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()

	// second's Serve must NOT start while first is still blocked: the loop is
	// single-flight, so Accept for c2 has not even been called yet.
	select {
	case <-second.started:
		t.Fatal("second connection's Serve started before the first one finished; the loop is not single-flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(first.finish)

	select {
	case <-second.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second connection never reached Serve after the first finished")
	}
	close(second.finish)

	deadline := time.After(2 * time.Second)
	for {
		if len(sequence) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("sequence = %v, want [1 2]", sequence)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if sequence[0] != 1 || sequence[1] != 2 {
		t.Fatalf("sequence = %v, want [1 2] (clients served one at a time, in order)", sequence)
	}
}

func TestUatWorkerRefusesLooseStateDirBeforeListening(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// 0755, not 0700: ValidateSocketDir must refuse this before ever calling
	// net.Listen, proving the hardening runs ahead of the socket syscalls.
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}

	err := runUatWorker([]string{"--repo", repo, "--state", state, "--session", "abcd1234"})
	if err == nil {
		t.Fatal("expected runUatWorker to refuse a 0755 session state directory")
	}
}
