package hosttrust

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Subject: two kinds sharing a Root never collide ------------------------

// TestSubject_DifferentKindsSameRootDoNotCollide is the load-bearing property
// of the opaque-subject design: a pack and a future environment record
// addressing the SAME canonical root must land at different keys in an
// AcceptanceStore, so accepting one can never read back as acceptance of
// the other.
func TestSubject_DifferentKindsSameRootDoNotCollide(t *testing.T) {
	root := CanonicalRoot("/tmp/shared-root")
	pack := Subject{Kind: "pack", Root: root}
	env := Subject{Kind: "environment", Root: root}

	if pack.Key() == env.Key() {
		t.Fatalf("Subject{pack,%q}.Key() == Subject{environment,%q}.Key() (%q); different kinds at the same root must never collide",
			root, root, pack.Key())
	}

	store := &AcceptanceStore{}
	store.Put(pack, Record{Fingerprint: "pack-fp"})
	store.Put(env, Record{Fingerprint: "env-fp"})

	gotPack, ok := store.Get(pack)
	if !ok || gotPack.Fingerprint != "pack-fp" {
		t.Errorf("pack record = (%+v, %v), want (pack-fp, true)", gotPack, ok)
	}
	gotEnv, ok := store.Get(env)
	if !ok || gotEnv.Fingerprint != "env-fp" {
		t.Errorf("environment record = (%+v, %v), want (env-fp, true)", gotEnv, ok)
	}
	if len(store.Accepted) != 2 {
		t.Errorf("want 2 distinct entries for 2 kinds at 1 root, got %d: %+v", len(store.Accepted), store.Accepted)
	}
}

// TestSubject_SameKindSameRootOverwrites: the SAME subject re-accepted
// overwrites its own prior record rather than accumulating — acceptance is
// always for the CURRENT surface only.
func TestSubject_SameKindSameRootOverwrites(t *testing.T) {
	subj := Subject{Kind: "pack", Root: CanonicalRoot("/tmp/one-root")}
	store := &AcceptanceStore{}
	store.Put(subj, Record{Fingerprint: "old"})
	store.Put(subj, Record{Fingerprint: "new"})
	got, ok := store.Get(subj)
	if !ok || got.Fingerprint != "new" {
		t.Fatalf("Get() = (%+v, %v), want (new, true)", got, ok)
	}
	if len(store.Accepted) != 1 {
		t.Fatalf("re-accepting the same subject must overwrite, not accumulate; got %d entries", len(store.Accepted))
	}
}

// TestAcceptanceStore_GetOnNilOrEmptyIsFalse: an absent or zero-fingerprint
// record is reported as not-accepted, never a zero-value forgery.
func TestAcceptanceStore_GetOnNilOrEmptyIsFalse(t *testing.T) {
	var nilStore *AcceptanceStore
	if _, ok := nilStore.Get(Subject{Kind: "pack", Root: "/x"}); ok {
		t.Error("a nil store must report not-accepted")
	}
	empty := &AcceptanceStore{}
	if _, ok := empty.Get(Subject{Kind: "pack", Root: "/x"}); ok {
		t.Error("an empty store must report not-accepted")
	}
	blankFP := &AcceptanceStore{Accepted: map[string]Record{Subject{Kind: "pack", Root: "/x"}.Key(): {Fingerprint: ""}}}
	if _, ok := blankFP.Get(Subject{Kind: "pack", Root: "/x"}); ok {
		t.Error("a record with an empty fingerprint must not count as accepted")
	}
}

// --- CanonicalRoot -----------------------------------------------------------

func TestCanonicalRoot_CleansUpRelativeSegments(t *testing.T) {
	a := CanonicalRoot("/tmp/x/y/../y/work")
	b := CanonicalRoot("/tmp/x/y/work")
	if a != b {
		t.Errorf("CanonicalRoot(%q) = %q, want it to equal CanonicalRoot(%q) = %q", "/tmp/x/y/../y/work", a, "/tmp/x/y/work", b)
	}
}

func TestCanonicalRoot_EmptyIsEmpty(t *testing.T) {
	if got := CanonicalRoot("   "); got != "" {
		t.Errorf("CanonicalRoot of blank input = %q, want empty", got)
	}
}

// --- Fingerprint --------------------------------------------------------------

// TestFingerprint_DeterministicAndSensitiveToContent: the same value hashes
// identically every time, and a changed value hashes differently — the two
// properties an acceptance fingerprint depends on.
func TestFingerprint_DeterministicAndSensitiveToContent(t *testing.T) {
	type doc struct {
		V     int      `json:"v"`
		Names []string `json:"names"`
	}
	a := doc{V: 1, Names: []string{"x", "y"}}
	fp1, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("Fingerprint is not deterministic: %q != %q", fp1, fp2)
	}
	b := doc{V: 1, Names: []string{"x", "y", "z"}}
	fp3, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp3 {
		t.Error("Fingerprint did not change when the content changed")
	}
}

// TestFingerprint_IsSha256OfCanonicalJSON pins the ENCODING, not just its
// determinism: a future edit that swaps the hash algorithm or the encoding
// step re-gates every already-accepted surface silently unless this fails.
func TestFingerprint_IsSha256OfCanonicalJSON(t *testing.T) {
	v := struct {
		A int    `json:"a"`
		B string `json:"b"`
	}{A: 1, B: "x"}
	got, err := Fingerprint(v)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Fingerprint(json.RawMessage(enc))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Fingerprint(v) = %q, want %q (sha256 of v's own json.Marshal)", got, want)
	}
}

// --- HashFile / IsSymlink -----------------------------------------------------

func TestHashFile_DeterministicOnContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha1, err := HashFile(path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	sha2, err := HashFile(path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Errorf("HashFile is not deterministic: %q != %q", sha1, sha2)
	}
	if err := os.WriteFile(path, []byte("goodbye"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha3, err := HashFile(path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	if sha1 == sha3 {
		t.Error("HashFile did not change when the file's content changed")
	}
}

// TestHashFile_RefusesSymlink is the content-hashing half of the trust gate's
// fail-closed posture: a symlinked target must never be silently followed and
// hashed as if it were the reviewed file.
func TestHashFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !IsSymlink(link) {
		t.Fatal("setup: IsSymlink must report the link as a symlink")
	}
	if IsSymlink(target) {
		t.Fatal("setup: IsSymlink must not report the real file as a symlink")
	}
	if _, err := HashFile(link, "linked file"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("HashFile(link) = %v, want a symlink-refusal error", err)
	}
}

func TestHashBytes_MatchesHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	data := []byte("snapshot bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	viaFile, err := HashFile(path, "test file")
	if err != nil {
		t.Fatal(err)
	}
	if got := HashBytes(data); got != viaFile {
		t.Errorf("HashBytes(data) = %q, want it to equal HashFile of the same bytes = %q", got, viaFile)
	}
}

func TestHashFile_MissingFileFailsClosed(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "absent"), "missing file"); err == nil {
		t.Error("HashFile of a missing path must return an error, not a zero-value hash")
	}
}

// --- SaveDocument / ReadDocumentBytes: atomic write + symlink refusal -------

type testDoc struct {
	Version int    `json:"version"`
	Note    string `json:"note"`
}

func TestSaveDocument_WritesAtomicallyAndReadable(t *testing.T) {
	dir := t.TempDir()
	doc := testDoc{Version: 1, Note: "hello"}
	if err := SaveDocument(dir, "doc.json", doc); err != nil {
		t.Fatal(err)
	}
	b, err := ReadDocumentBytes(filepath.Join(dir, "doc.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got testDoc
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != doc {
		t.Errorf("round-tripped doc = %+v, want %+v", got, doc)
	}
	// No .tmp- leftovers: AtomicWriteInDir renames into place, never leaves a
	// partial file behind on success.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "doc.json" {
		t.Errorf("dir contents = %v, want exactly [doc.json]", ents)
	}
}

func TestSaveDocument_RefusesSymlinkedDestination(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "doc.json")
	if err := os.Symlink(elsewhere, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := SaveDocument(dir, "doc.json", testDoc{Version: 1}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("SaveDocument through a symlinked destination = %v, want a symlink-refusal error", err)
	}
}

func TestReadDocumentBytes_RefusesSymlinkedSource(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"accepted":{"path:/evil":{"fingerprint":"aaaa"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadDocumentBytes(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("ReadDocumentBytes(link) = %v, want a symlink-refusal error", err)
	}
}

func TestReadDocumentBytes_AbsentIsPlainNotExist(t *testing.T) {
	_, err := ReadDocumentBytes(filepath.Join(t.TempDir(), "absent.json"))
	if !os.IsNotExist(err) {
		t.Errorf("ReadDocumentBytes of an absent path = %v, want os.IsNotExist true so a caller can supply its own fresh default", err)
	}
}

// --- WithLock: cross-process serialization + concurrent (non-nested) safety --

// TestWithLock_SerializesConcurrentWriters proves WithLock actually excludes:
// N goroutines racing the SAME lockPath each get their turn (none refused,
// none interleaved) — the property mutatePackTrustStore's callers depend on.
func TestWithLock_SerializesConcurrentWriters(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "store.lock")
	const writers = 8
	var mu sync.Mutex
	inCritical := 0
	maxObserved := 0
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WithLock(lockPath, func() error {
				mu.Lock()
				inCritical++
				if inCritical > maxObserved {
					maxObserved = inCritical
				}
				mu.Unlock()
				mu.Lock()
				inCritical--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("writer failed: %v", err)
		}
	}
	if maxObserved > 1 {
		t.Errorf("observed %d writers inside the critical section at once, want at most 1 (WithLock did not serialize)", maxObserved)
	}
}

// TestWithLock_ReportsTheCriticalSectionsOwnError: the lock is transport, not
// policy — fn's own error reaches the caller unwrapped.
func TestWithLock_ReportsTheCriticalSectionsOwnError(t *testing.T) {
	want := errors.New("the section failed")
	lockPath := filepath.Join(t.TempDir(), "free.lock")
	if got := WithLock(lockPath, func() error { return want }); !errors.Is(got, want) {
		t.Errorf("WithLock returned %v, want the section's own error", got)
	}
}

// --- LoadMutateSave: fresh-load -> mutate -> save, and lock ownership -------

// TestLoadMutateSave_LoadMutateSaveOrder proves the shape itself: mutate sees
// what load produced, and save receives what mutate left behind.
func TestLoadMutateSave_LoadMutateSaveOrder(t *testing.T) {
	var order []string
	got, err := LoadMutateSave(
		func() (*testDoc, error) { order = append(order, "load"); return &testDoc{Version: 1}, nil },
		func(d *testDoc) error { order = append(order, "mutate"); d.Note = "mutated"; return nil },
		func(d *testDoc) error {
			order = append(order, "save")
			if d.Note != "mutated" {
				t.Errorf("save saw Note=%q, want the mutate step's write to be visible", d.Note)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "mutated" {
		t.Errorf("LoadMutateSave returned Note=%q, want mutated", got.Note)
	}
	if strings.Join(order, ",") != "load,mutate,save" {
		t.Errorf("call order = %v, want [load mutate save]", order)
	}
}

// TestLoadMutateSave_MutateErrorSkipsSave: a failed mutation must never reach
// save — the caller's document write, if any, must not fire.
func TestLoadMutateSave_MutateErrorSkipsSave(t *testing.T) {
	saveCalled := false
	_, err := LoadMutateSave(
		func() (*testDoc, error) { return &testDoc{}, nil },
		func(*testDoc) error { return errors.New("boom") },
		func(*testDoc) error { saveCalled = true; return nil },
	)
	if err == nil {
		t.Fatal("want an error from a failing mutate")
	}
	if saveCalled {
		t.Error("save must not run after mutate fails")
	}
}

// TestLoadMutateSave_LoadErrorSkipsMutateAndSave: a failed load must never
// reach mutate or save.
func TestLoadMutateSave_LoadErrorSkipsMutateAndSave(t *testing.T) {
	mutateCalled, saveCalled := false, false
	_, err := LoadMutateSave(
		func() (*testDoc, error) { return nil, errors.New("unreadable") },
		func(*testDoc) error { mutateCalled = true; return nil },
		func(*testDoc) error { saveCalled = true; return nil },
	)
	if err == nil {
		t.Fatal("want an error from a failing load")
	}
	if mutateCalled || saveCalled {
		t.Error("mutate/save must not run after load fails")
	}
}

// TestLoadMutateSave_NeverAcquiresALock is the "by construction" proof for
// the documented nesting invariant: LoadMutateSave is what a caller runs FROM
// INSIDE WithLock's fn, so if it could itself acquire a lock, calling it
// would self-deadlock (or time out against sys's bounded wait) the moment
// someone reached for "just lock again to be safe". Rather than trust the
// doc comment, this walks mutate.go's own source and fails if it references
// any lock-acquiring symbol or imports the sys package at all — nesting
// cannot happen because the code that would nest has no path to a lock,
// not because nobody has tried yet.
func TestLoadMutateSave_NeverAcquiresALock(t *testing.T) {
	const path = "mutate.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if p == "pix/host/sys" {
			t.Fatalf("%s imports %q; the fresh-load->mutate->save shape callers run WHILE HOLDING the lock must never be able to reach a second lock acquisition", path, p)
		}
	}
	forbidden := map[string]bool{"WithLock": true, "WithLockNotifying": true}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
			found = append(found, id.Name)
		}
		return true
	})
	if len(found) > 0 {
		t.Fatalf("%s references %v — a lock-acquiring symbol inside the operation callers run while ALREADY HOLDING the lock is a self-deadlock waiting to happen; nested acquisition must be impossible by construction, not by convention", path, found)
	}
}

// TestLoadMutateSave_ComposesWithWithLockWithoutDeadlock is the end-to-end
// proof: the sanctioned pattern (WithLock wrapping LoadMutateSave) actually
// runs to completion promptly, rather than the reader having to trust that
// the two primitives compose.
func TestLoadMutateSave_ComposesWithWithLockWithoutDeadlock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "compose.lock")
	done := make(chan error, 1)
	go func() {
		done <- WithLock(lockPath, func() error {
			_, err := LoadMutateSave(
				func() (*testDoc, error) { return &testDoc{}, nil },
				func(d *testDoc) error { d.Note = "ok"; return nil },
				func(*testDoc) error { return nil },
			)
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithLock+LoadMutateSave failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WithLock(LoadMutateSave(...)) never returned")
	}
}
