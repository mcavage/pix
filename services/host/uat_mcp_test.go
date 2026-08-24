package main

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"
)

// fakeConn is a minimal io.ReadWriteCloser double for relayBytes: it lets a
// test drive both the "worker" side (what relayBytes reads/writes as conn)
// without a real socket.
type fakeConn struct {
	r      io.Reader
	w      *bytes.Buffer
	closed bool
}

func (f *fakeConn) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeConn) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *fakeConn) Close() error                { f.closed = true; return nil }

func TestRelayBytesCopiesBothDirections(t *testing.T) {
	clientToWorker := strings.NewReader("request-from-mcp-client")
	var toClient bytes.Buffer
	conn := &fakeConn{r: strings.NewReader("response-from-worker"), w: &toClient}

	var fromWorker bytes.Buffer
	err := relayBytes(clientToWorker, &fromWorker, conn)
	if err != nil {
		t.Fatalf("relayBytes: %v", err)
	}
	if toClient.String() != "request-from-mcp-client" {
		t.Errorf("bytes relayed stdin->conn = %q, want %q", toClient.String(), "request-from-mcp-client")
	}
	if fromWorker.String() != "response-from-worker" {
		t.Errorf("bytes relayed conn->stdout = %q, want %q", fromWorker.String(), "response-from-worker")
	}
	if !conn.closed {
		t.Error("relayBytes must close conn once a direction reaches EOF")
	}
}

func TestRelayBytesPropagatesConnReadError(t *testing.T) {
	boom := errors.New("boom")
	conn := &fakeConn{r: errReader{boom}, w: &bytes.Buffer{}}
	var out bytes.Buffer
	err := relayBytes(strings.NewReader(""), &out, conn)
	if err == nil {
		t.Fatal("expected relayBytes to surface the conn read error")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestUatMcpGatewayIsADumbRelay is the structural sentinel for the U1 split
// (docs/design/self-development-uat.md): `pix-host uat-mcp`, spawned by the
// sbx gateway, must never regain the ability to construct a UAT Runner or
// execute host commands directly — that authority moved to `pix-host
// uat-worker`, started later by `pix run --dev` so it inherits the operator's
// authenticated host context. A source-level import check is deliberately
// blunt (mirroring cmd/pix/hostmode_gone_test.go's identical rationale): it
// catches the regression the moment either forbidden import reappears in this
// file, wherever in it that happens.
func TestUatMcpGatewayIsADumbRelay(t *testing.T) {
	violations := uatMcpForbiddenImports(t, "uat_mcp.go")
	for _, v := range violations {
		t.Errorf("uat_mcp.go imports %q; the gateway relay must never construct a Runner or exec host commands — that moved to uat-worker (docs/design/self-development-uat.md)", v)
	}
}

// TestUatMcpSentinelDetectsAPlantedViolation proves the check above actually
// fires, the same discipline hostmode_gone_test.go's plausibility test
// applies: a guard that has only ever been seen passing has never been proven
// to catch anything.
func TestUatMcpSentinelDetectsAPlantedViolation(t *testing.T) {
	dir := t.TempDir()
	planted := dir + "/planted.go"
	src := "package main\n\nimport (\n\t\"os/exec\"\n\n\t_ \"pix/host/workflow/uat\"\n)\n\nvar _ = exec.Command\n"
	if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	violations := uatMcpForbiddenImports(t, planted)
	if len(violations) != 2 {
		t.Fatalf("expected 2 planted violations (os/exec, workflow/uat), got %v", violations)
	}
}

func uatMcpForbiddenImports(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	forbidden := []string{"os/exec", "pix/host/workflow/uat"}
	var found []string
	for _, spec := range f.Imports {
		p := strings.Trim(spec.Path.Value, `"`)
		for _, bad := range forbidden {
			if p == bad {
				found = append(found, bad)
			}
		}
	}
	return found
}
