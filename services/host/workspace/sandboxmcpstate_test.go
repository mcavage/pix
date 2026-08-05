// sandboxmcpstate_test.go — coverage for sandboxmcpstate.go: the launcher-owned
// per-sandbox MCP receipt (<stateDir>/sandboxes/<sandbox>/mcp.json).
package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// GWServerName is a literal here rather than an import: a receipt records
// whatever server names it is handed, and this package has no business knowing
// that one of them belongs to Google Workspace. Using the real constant would
const GWServerName = "google-workspace"

func fixedClock(ts string) func() time.Time {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}

// --- roundtrip -------------------------------------------------------------

func TestWriteCreateReceiptRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCreateReceipt(dir, "pix-work", "", []string{"slack", GWServerName}, fixedClock("2024-01-02T03:04:05Z")); err != nil {
		t.Fatalf("WriteCreateReceipt: %v", err)
	}
	r, status, err := ReadMCPReceipt(dir, "pix-work")
	if err != nil {
		t.Fatalf("ReadMCPReceipt: %v", err)
	}
	if status != MCPStateOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if r.Sandbox != "pix-work" {
		t.Errorf("Sandbox = %q", r.Sandbox)
	}
	if r.CreatedAt != "2024-01-02T03:04:05Z" {
		t.Errorf("CreatedAt = %q", r.CreatedAt)
	}
	if len(r.Preloaded) != 2 || r.Preloaded[0] != "slack" || r.Preloaded[1] != GWServerName {
		t.Errorf("Preloaded = %v", r.Preloaded)
	}
	if len(r.Loads) != 0 {
		t.Errorf("Loads = %v, want none", r.Loads)
	}

	// Underlying file: schema 1, present at the documented path.
	raw, err := os.ReadFile(filepath.Join(dir, "sandboxes", "pix-work", "mcp.json"))
	if err != nil {
		t.Fatalf("reading raw receipt: %v", err)
	}
	var raw2 map[string]any
	if err := json.Unmarshal(raw, &raw2); err != nil {
		t.Fatalf("raw receipt not valid JSON: %v", err)
	}
	if int(raw2["schema"].(float64)) != 1 {
		t.Errorf("raw schema = %v, want 1", raw2["schema"])
	}
}

func TestAppendLoadReceiptRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCreateReceipt(dir, "pix-work", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatalf("WriteCreateReceipt: %v", err)
	}
	if err := AppendLoadReceipt(dir, "pix-work", GWServerName, fixedClock("2024-01-01T01:00:00Z")); err != nil {
		t.Fatalf("AppendLoadReceipt: %v", err)
	}
	r, status, err := ReadMCPReceipt(dir, "pix-work")
	if err != nil || status != MCPStateOK {
		t.Fatalf("read: status=%v err=%v", status, err)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != GWServerName || r.Loads[0].At != "2024-01-01T01:00:00Z" {
		t.Fatalf("Loads = %+v", r.Loads)
	}
	// Preloaded/CreatedAt from the create receipt untouched by the append.
	if r.CreatedAt != "2024-01-01T00:00:00Z" || len(r.Preloaded) != 1 || r.Preloaded[0] != "slack" {
		t.Fatalf("create fields disturbed by append: %+v", r)
	}
}

// --- order + dedupe ---------------------------------------------------------

func TestAppendLoadReceiptOrderAndDedupe(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-order"
	if err := WriteCreateReceipt(dir, sandbox, "", nil, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := AppendLoadReceipt(dir, sandbox, "slack", fixedClock("2024-01-01T01:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := AppendLoadReceipt(dir, sandbox, "notion", fixedClock("2024-01-01T02:00:00Z")); err != nil {
		t.Fatal(err)
	}
	// Re-loading "slack" later must NOT move it or bump its timestamp.
	if err := AppendLoadReceipt(dir, sandbox, "slack", fixedClock("2024-01-01T03:00:00Z")); err != nil {
		t.Fatal(err)
	}
	r, _, err := ReadMCPReceipt(dir, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Loads) != 2 {
		t.Fatalf("Loads = %+v, want 2 entries (deduped)", r.Loads)
	}
	if r.Loads[0].Name != "slack" || r.Loads[0].At != "2024-01-01T01:00:00Z" {
		t.Fatalf("Loads[0] = %+v, want first-seen slack@01:00", r.Loads[0])
	}
	if r.Loads[1].Name != "notion" || r.Loads[1].At != "2024-01-01T02:00:00Z" {
		t.Fatalf("Loads[1] = %+v, want notion@02:00", r.Loads[1])
	}
}

// --- replace resets loads ----------------------------------------------------

func TestWriteCreateReceiptReplaceResetsLoads(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-replace"
	if err := WriteCreateReceipt(dir, sandbox, "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := AppendLoadReceipt(dir, sandbox, "slack", fixedClock("2024-01-01T01:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := AppendLoadReceipt(dir, sandbox, "notion", fixedClock("2024-01-01T02:00:00Z")); err != nil {
		t.Fatal(err)
	}
	// Recreate (e.g. `sbx rm -f` + fresh create) with a different preload set.
	if err := WriteCreateReceipt(dir, sandbox, "", []string{GWServerName}, fixedClock("2024-02-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	r, status, err := ReadMCPReceipt(dir, sandbox)
	if err != nil || status != MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.CreatedAt != "2024-02-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want the fresh create time", r.CreatedAt)
	}
	if len(r.Preloaded) != 1 || r.Preloaded[0] != GWServerName {
		t.Errorf("Preloaded = %v, want [gog]", r.Preloaded)
	}
	if len(r.Loads) != 0 {
		t.Errorf("Loads = %v, want reset to none after replace", r.Loads)
	}
}

// --- partial legacy receipt --------------------------------------------------

func TestAppendLoadReceiptOldSandboxCreatesPartialReceipt(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-legacy"
	// No WriteCreateReceipt ever called — simulates a sandbox created before
	// this feature shipped.
	if err := AppendLoadReceipt(dir, sandbox, "slack", fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatalf("AppendLoadReceipt on old sandbox: %v", err)
	}
	r, status, err := ReadMCPReceipt(dir, sandbox)
	if err != nil || status != MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if r.CreatedAt != "" {
		t.Errorf("CreatedAt = %q, want empty for a synthesized partial receipt", r.CreatedAt)
	}
	if len(r.Preloaded) != 0 {
		t.Errorf("Preloaded = %v, want none for a synthesized partial receipt", r.Preloaded)
	}
	if len(r.Loads) != 1 || r.Loads[0].Name != "slack" {
		t.Errorf("Loads = %+v, want [slack]", r.Loads)
	}
}

// --- absent ------------------------------------------------------------------

func TestReadSandboxMCPReceiptAbsent(t *testing.T) {
	dir := t.TempDir()
	r, status, err := ReadMCPReceipt(dir, "pix-never-touched")
	if err != nil {
		t.Fatalf("absent read returned error: %v", err)
	}
	if status != MCPStateAbsent {
		t.Fatalf("status = %v, want absent", status)
	}
	if r != nil {
		t.Fatalf("r = %+v, want nil", r)
	}
	if status.Unverifiable() {
		t.Fatal("absent must not be Unverifiable — it is legitimately empty")
	}
}

// --- corruption / schema / identity ------------------------------------------

func TestReadSandboxMCPReceiptCorrupt(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-corrupt"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "mcp.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, status, err := ReadMCPReceipt(dir, sandbox)
	if err == nil {
		t.Fatal("want an error for corrupt JSON")
	}
	if status != MCPStateCorrupt {
		t.Fatalf("status = %v, want corrupt", status)
	}
	if r != nil {
		t.Fatalf("r = %+v, want nil on corrupt read", r)
	}
	if !status.Unverifiable() {
		t.Fatal("corrupt must be Unverifiable")
	}
}

func TestReadSandboxMCPReceiptSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-schema"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":99,"sandbox":"pix-schema"}`
	if err := os.WriteFile(filepath.Join(sdir, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, status, err := ReadMCPReceipt(dir, sandbox)
	if err == nil {
		t.Fatal("want an error for schema mismatch")
	}
	if status != MCPStateSchemaMismatch {
		t.Fatalf("status = %v, want schema-mismatch", status)
	}
	if !status.Unverifiable() {
		t.Fatal("schema mismatch must be Unverifiable")
	}
}

func TestReadSandboxMCPReceiptIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-identity"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"sandbox":"some-other-sandbox"}`
	if err := os.WriteFile(filepath.Join(sdir, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, status, err := ReadMCPReceipt(dir, sandbox)
	if err == nil {
		t.Fatal("want an error for identity mismatch")
	}
	if status != MCPStateIdentityMismatch {
		t.Fatalf("status = %v, want identity-mismatch", status)
	}
	if !status.Unverifiable() {
		t.Fatal("identity mismatch must be Unverifiable")
	}
}

// AppendLoadReceipt must FAIL CLOSED (never silently clobber) against an
// unverifiable existing receipt.
func TestAppendLoadReceiptFailsClosedOnUnverifiable(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-badread"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "mcp.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := AppendLoadReceipt(dir, sandbox, "slack", fixedClock("2024-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("want AppendLoadReceipt to refuse an unverifiable existing receipt")
	}
	// The corrupt file must be left untouched, not clobbered.
	raw, rerr := os.ReadFile(filepath.Join(sdir, "mcp.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(raw) != "{not json" {
		t.Fatalf("corrupt receipt was overwritten: %q", raw)
	}
}

// --- traversal ----------------------------------------------------------------

func TestSandboxNameTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	bad := []string{"", ".", "..", "../escape", "a/../../b", "foo/bar", "foo\\bar", "/etc/passwd"}
	for _, name := range bad {
		if err := WriteCreateReceipt(dir, name, "", nil, fixedClock("2024-01-01T00:00:00Z")); err == nil {
			t.Errorf("WriteCreateReceipt(%q): want error, got nil", name)
		}
		if err := AppendLoadReceipt(dir, name, "slack", fixedClock("2024-01-01T00:00:00Z")); err == nil {
			t.Errorf("AppendLoadReceipt(%q): want error, got nil", name)
		}
		if _, _, err := ReadMCPReceipt(dir, name); err == nil {
			t.Errorf("ReadMCPReceipt(%q): want error, got nil", name)
		}
	}
	// Confirm nothing escaped the state root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Fatal("traversal escaped the state root")
	}
}

// --- symlink destination / parent --------------------------------------------

func TestWriteCreateReceiptRefusesSymlinkedSandboxDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "sandboxes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	RequireSymlink(t, outside, filepath.Join(root, "pix-sym"))

	if err := WriteCreateReceipt(dir, "pix-sym", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err == nil {
		t.Fatal("want an error writing through a symlinked sandbox directory")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("write followed the symlink into %s: %v", outside, entries)
	}
}

func TestWriteCreateReceiptRefusesSymlinkedStateRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	RequireSymlink(t, outside, filepath.Join(dir, "sandboxes"))

	if err := WriteCreateReceipt(dir, "pix-x", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err == nil {
		t.Fatal("want an error creating through a symlinked state root")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("write followed the symlinked root into %s: %v", outside, entries)
	}
}

// A hostile symlinked mcp.json (the destination file itself) must be REPLACED
// by an atomic rename, never followed/truncated-through.
func TestWriteCreateReceiptReplacesSymlinkedDestinationFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	sandbox := "pix-dest-sym"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	RequireSymlink(t, victim, filepath.Join(sdir, "mcp.json"))

	if err := WriteCreateReceipt(dir, sandbox, "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatalf("WriteCreateReceipt: %v", err)
	}
	// The victim file must be untouched...
	vb, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(vb) != "do-not-touch" {
		t.Fatalf("victim file was written through: %q", vb)
	}
	// ...and mcp.json is now a real file with the receipt, not a symlink.
	fi, err := os.Lstat(filepath.Join(sdir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("mcp.json is still a symlink after write")
	}
	r, status, err := ReadMCPReceipt(dir, sandbox)
	if err != nil || status != MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Preloaded) != 1 || r.Preloaded[0] != "slack" {
		t.Fatalf("Preloaded = %v", r.Preloaded)
	}
}

// ReadMCPReceipt must refuse (not follow) a symlinked mcp.json too.
func TestReadSandboxMCPReceiptRefusesSymlinkedDestinationFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	sandbox := "pix-read-sym"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte(`{"schema":1,"sandbox":"pix-read-sym"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	RequireSymlink(t, elsewhere, filepath.Join(sdir, "mcp.json"))

	_, status, err := ReadMCPReceipt(dir, sandbox)
	if err == nil {
		t.Fatal("want an error reading through a symlinked destination")
	}
	if status != MCPStateUnreadable {
		t.Fatalf("status = %v, want unreadable", status)
	}
}

// --- concurrent appends -------------------------------------------------------

func TestAppendLoadReceiptConcurrentAppendsDoNotLoseUpdates(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-concurrent"
	if err := WriteCreateReceipt(dir, sandbox, "", nil, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		names = append(names, "server-"+string(rune('a'+i)))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			errs[i] = AppendLoadReceipt(dir, sandbox, name, fixedClock("2024-01-01T01:00:00Z"))
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("AppendLoadReceipt(%q): %v", names[i], err)
		}
	}
	r, status, err := ReadMCPReceipt(dir, sandbox)
	if err != nil || status != MCPStateOK {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if len(r.Loads) != len(names) {
		t.Fatalf("Loads has %d entries, want %d (a concurrent append was lost): %+v", len(r.Loads), len(names), r.Loads)
	}
	seen := map[string]bool{}
	for _, l := range r.Loads {
		if seen[l.Name] {
			t.Fatalf("duplicate load entry for %q", l.Name)
		}
		seen[l.Name] = true
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("missing load entry for %q", name)
		}
	}
}

// --- permissions ---------------------------------------------------------------

func TestSandboxMCPStatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix permission bits don't apply on windows")
	}
	dir := t.TempDir()
	sandbox := "pix-perm"
	if err := WriteCreateReceipt(dir, sandbox, "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	root := MCPStateRoot(dir)
	if fi, err := os.Stat(root); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("root perm = %o, want 700", fi.Mode().Perm())
	}
	sdir := filepath.Join(root, sandbox)
	if fi, err := os.Stat(sdir); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("sandbox dir perm = %o, want 700", fi.Mode().Perm())
	}
	file := filepath.Join(sdir, "mcp.json")
	if fi, err := os.Stat(file); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("mcp.json perm = %o, want 600", fi.Mode().Perm())
	}
}

// --- misc: empty name to AppendLoadReceipt, nil-clock default, status strings ---

func TestAppendLoadReceiptRejectsEmptyName(t *testing.T) {
	dir := t.TempDir()
	if err := AppendLoadReceipt(dir, "pix-empty-name", "   ", fixedClock("2024-01-01T00:00:00Z")); err == nil {
		t.Fatal("want an error for a blank mcp server name")
	}
}

func TestWriteAndAppendDefaultClock(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Add(-time.Second)
	if err := WriteCreateReceipt(dir, "pix-defclock", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := AppendLoadReceipt(dir, "pix-defclock", "slack", nil); err != nil {
		t.Fatal(err)
	}
	r, _, err := ReadMCPReceipt(dir, "pix-defclock")
	if err != nil {
		t.Fatal(err)
	}
	createdAt, err := time.Parse(time.RFC3339, r.CreatedAt)
	if err != nil {
		t.Fatalf("CreatedAt %q not RFC3339: %v", r.CreatedAt, err)
	}
	if createdAt.Before(before) {
		t.Fatalf("CreatedAt %v looks stale relative to %v", createdAt, before)
	}
}

func TestSandboxMCPStateStatusStringsAndUnverifiable(t *testing.T) {
	cases := []struct {
		status       MCPStateStatus
		want         string
		unverifiable bool
	}{
		{MCPStateOK, "ok", false},
		{MCPStateAbsent, "absent", false},
		{MCPStateUnreadable, "unreadable", true},
		{MCPStateCorrupt, "corrupt", true},
		{MCPStateSchemaMismatch, "schema-mismatch", true},
		{MCPStateIdentityMismatch, "identity-mismatch", true},
	}
	for _, c := range cases {
		if got := c.status.String(); got != c.want {
			t.Errorf("%v.String() = %q, want %q", int(c.status), got, c.want)
		}
		if got := c.status.Unverifiable(); got != c.unverifiable {
			t.Errorf("%v.Unverifiable() = %v, want %v", c.want, got, c.unverifiable)
		}
	}
}

func TestValidateSandboxStateNameAcceptsRealisticNames(t *testing.T) {
	good := []string{"pix-work", "pix-t-foo-abc123", "a", "A1_-"}
	for _, name := range good {
		if err := ValidateStateName(name); err != nil {
			t.Errorf("ValidateStateName(%q): %v", name, err)
		}
	}
}

// Sanity: errors.Is-style — ensure the error returned actually mentions
// something identifiable rather than being empty/generic, since doctor/status
// will surface it.
func TestReadSandboxMCPReceiptErrorMessagesAreDescriptive(t *testing.T) {
	dir := t.TempDir()
	sandbox := "pix-msg"
	sdir := filepath.Join(dir, "sandboxes", sandbox)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "mcp.json"), []byte(`{"schema":1,"sandbox":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadMCPReceipt(dir, sandbox)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("err = %v, want a message mentioning identity", err)
	}
	var perr *os.PathError
	if errors.As(err, &perr) {
		t.Fatalf("unexpected raw *os.PathError leaking as the identity-mismatch error: %v", err)
	}
}

// RequireSymlink creates a symlink or skips on a platform that cannot. Each
// package carries its own three-line copy rather than importing a testing
// helper across a package boundary, which is the correct amount of duplication
