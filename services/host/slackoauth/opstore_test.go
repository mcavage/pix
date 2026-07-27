package slackoauth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// fakeRunnerCall records one invocation of the fake CommandRunner.
type fakeRunnerCall struct {
	stdin []byte
	name  string
	args  []string
}

// fakeRunner is an in-memory CommandRunner: it never shells out, just
// records every call and returns a scripted response (by call index).
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeRunnerCall
	outputs [][]byte
	errs    []error
}

func (r *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.calls)
	r.calls = append(r.calls, fakeRunnerCall{stdin: append([]byte(nil), stdin...), name: name, args: append([]string(nil), args...)})
	var out []byte
	var err error
	if idx < len(r.outputs) {
		out = r.outputs[idx]
	}
	if idx < len(r.errs) {
		err = r.errs[idx]
	}
	return out, err
}

func opBlobJSON(t *testing.T) []byte {
	t.Helper()
	data, err := MarshalBlob(validBlob())
	if err != nil {
		t.Fatalf("MarshalBlob: %v", err)
	}
	return data
}

// TestOPStoreReadRequiresExistingItem proves Read refuses cleanly when the
// store has no item yet (nothing has ever been written) instead of running a
// doomed `op document get` with an empty identifier.
func TestOPStoreReadRequiresExistingItem(t *testing.T) {
	r := &fakeRunner{}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "")
	if _, err := s.Read(context.Background()); err == nil {
		t.Fatal("Read succeeded with no item configured; want a refusal")
	}
	if len(r.calls) != 0 {
		t.Errorf("Read invoked the runner with no item; want zero calls, got %d", len(r.calls))
	}
}

// TestOPStoreReadInvokesOpDocumentGet proves Read shells out to exactly
// `op document get ITEM --vault VAULT` with no stdin, and parses the result
// as a v1 Blob.
func TestOPStoreReadInvokesOpDocumentGet(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{opBlobJSON(t)}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")

	b, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if b.TeamID != "T0123" {
		t.Errorf("TeamID = %q, want T0123", b.TeamID)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	call := r.calls[0]
	if call.name != "op" {
		t.Errorf("name = %q, want op", call.name)
	}
	wantArgs := []string{"document", "get", "item123", "--vault", "MyVault"}
	if strings.Join(call.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
	if len(call.stdin) != 0 {
		t.Errorf("Read sent stdin %q; want none", call.stdin)
	}
}

// TestOPStoreWriteCreatesWhenNoItemYet proves the first Write against a
// store with no known item shells out to `op document create - --vault
// VAULT --title TITLE --format json`, sends the blob JSON ONLY over stdin,
// and learns the created item id from the JSON response.
func TestOPStoreWriteCreatesWhenNoItemYet(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{[]byte(`{"id":"new-item-id","vault":{"id":"vault-id-1"}}`)}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "")

	b := validBlob()
	if err := s.Write(context.Background(), b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	call := r.calls[0]
	wantArgs := []string{"document", "create", "-", "--vault", "MyVault", "--title", "pix-slack-oauth", "--format", "json"}
	if strings.Join(call.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
	var got Blob
	if err := json.Unmarshal(call.stdin, &got); err != nil {
		t.Fatalf("stdin was not the blob JSON: %v", err)
	}
	if got.AccessToken != b.AccessToken {
		t.Errorf("stdin blob AccessToken = %q, want %q", got.AccessToken, b.AccessToken)
	}
	if s.ItemID() != "new-item-id" {
		t.Errorf("ItemID() = %q, want new-item-id (parsed from the create response)", s.ItemID())
	}
}

// TestOPStoreWriteEditsWhenItemKnown proves a Write after the item is known
// (whether from construction or a prior create) shells out to `op document
// edit ITEM - --vault VAULT --format json` instead of create.
func TestOPStoreWriteEditsWhenItemKnown(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{[]byte(`{"id":"item123","vault":{"id":"vault-id-1"}}`)}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")

	if err := s.Write(context.Background(), validBlob()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	call := r.calls[0]
	wantArgs := []string{"document", "edit", "item123", "-", "--vault", "MyVault", "--format", "json"}
	if strings.Join(call.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
}

// TestOPStoreVaultIDCapturedFromWriteResponse proves Write captures the
// ACTUAL vault id `op document create|edit --format json` returns — even
// when Vault was configured as a vault NAME rather than an id — so a caller
// (the PKCE setup flow) can persist the resolved vault id into config
// without a second round trip.
func TestOPStoreVaultIDCapturedFromWriteResponse(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{[]byte(`{"id":"new-item-id","vault":{"id":"vault-id-1"}}`)}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "")
	if got := s.VaultID(); got != "" {
		t.Errorf("VaultID() before any Write = %q, want empty", got)
	}
	if err := s.Write(context.Background(), validBlob()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := s.VaultID(); got != "vault-id-1" {
		t.Errorf("VaultID() = %q, want vault-id-1", got)
	}
}

// TestOPStoreWriteNeverPutsBlobOnArgv proves the credential JSON appears
// ONLY in stdin, never anywhere in argv, on both create and edit paths.
func TestOPStoreWriteNeverPutsBlobOnArgv(t *testing.T) {
	cases := []struct {
		name string
		item string
		out  string
	}{
		{"create", "", `{"id":"new-id","vault":{"id":"v1"}}`},
		{"edit", "item123", `{"id":"item123","vault":{"id":"v1"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{outputs: [][]byte{[]byte(tc.out)}}
			s := NewOPStore(r, "MyVault", "pix-slack-oauth", tc.item)
			b := validBlob()
			if err := s.Write(context.Background(), b); err != nil {
				t.Fatalf("Write: %v", err)
			}
			argv := strings.Join(r.calls[0].args, " ")
			for _, secret := range []string{b.AccessToken, b.RefreshToken} {
				if strings.Contains(argv, secret) {
					t.Errorf("argv %v contains a secret token; must be stdin-only", r.calls[0].args)
				}
			}
			if len(r.calls[0].stdin) == 0 {
				t.Error("Write sent no stdin; the blob must travel over stdin")
			}
		})
	}
}

// TestOPStoreWriteRejectsInvalidBlob proves an incomplete blob never reaches
// the command runner at all.
func TestOPStoreWriteRejectsInvalidBlob(t *testing.T) {
	r := &fakeRunner{}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")
	b := validBlob()
	b.AccessToken = ""
	if err := s.Write(context.Background(), b); err == nil {
		t.Fatal("Write succeeded with an invalid blob; want a validation error")
	}
	if len(r.calls) != 0 {
		t.Errorf("Write invoked the runner with an invalid blob; want zero calls, got %d", len(r.calls))
	}
}

// TestOPStoreErrorsDoNotLeakSecrets proves that when the runner fails, the
// returned error never contains the credential JSON that was sent on stdin.
func TestOPStoreErrorsDoNotLeakSecrets(t *testing.T) {
	r := &fakeRunner{errs: []error{&opFakeErr{"op: some diagnostic, no secrets here"}}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")
	b := validBlob()
	err := s.Write(context.Background(), b)
	if err == nil {
		t.Fatal("Write succeeded despite a runner error; want it to propagate")
	}
	if strings.Contains(err.Error(), b.AccessToken) || strings.Contains(err.Error(), b.RefreshToken) {
		t.Errorf("error leaked a secret token: %q", err.Error())
	}
}

type opFakeErr struct{ msg string }

func (e *opFakeErr) Error() string { return e.msg }

// TestOPStoreDeleteRequiresExistingItem proves Delete refuses cleanly when
// the store has no item yet, instead of running a doomed `op document
// delete` with an empty identifier.
func TestOPStoreDeleteRequiresExistingItem(t *testing.T) {
	r := &fakeRunner{}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "")
	if err := s.Delete(context.Background()); err == nil {
		t.Fatal("Delete succeeded with no item configured; want a refusal")
	}
	if len(r.calls) != 0 {
		t.Errorf("Delete invoked the runner with no item; want zero calls, got %d", len(r.calls))
	}
}

// TestOPStoreDeleteInvokesOpDocumentDeleteArchive proves Delete shells out
// to exactly `op document delete ITEM --vault VAULT --archive` with no
// stdin — the item and vault are non-secret identifiers, so this call never
// needs to carry anything on stdin, and it must never carry a credential on
// argv either (there is none to carry, but the shape is asserted anyway).
func TestOPStoreDeleteInvokesOpDocumentDeleteArchive(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{nil}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")
	if err := s.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	call := r.calls[0]
	if call.name != "op" {
		t.Errorf("name = %q, want op", call.name)
	}
	wantArgs := []string{"document", "delete", "item123", "--vault", "MyVault", "--archive"}
	if strings.Join(call.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
	if len(call.stdin) != 0 {
		t.Errorf("Delete sent stdin %q; want none", call.stdin)
	}
}

// TestOPStoreDeletePropagatesRunnerError proves a runner failure on delete
// propagates rather than being swallowed as a false success.
func TestOPStoreDeletePropagatesRunnerError(t *testing.T) {
	r := &fakeRunner{errs: []error{&opFakeErr{"op: some diagnostic"}}}
	s := NewOPStore(r, "MyVault", "pix-slack-oauth", "item123")
	if err := s.Delete(context.Background()); err == nil {
		t.Fatal("Delete succeeded despite a runner error; want it to propagate")
	}
}
