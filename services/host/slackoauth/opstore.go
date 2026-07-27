package slackoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// CommandRunner is the seam OPStore uses to invoke the `op` CLI. Production
// code uses ExecRunner; tests inject a fake that records argv/stdin without
// ever shelling out. Implementations MUST honor ctx cancellation/deadline and
// must NEVER embed stdin (which carries the credential JSON) into a returned
// error.
type CommandRunner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) (stdout []byte, err error)
}

// OPStore is a Store backed by a single 1Password document item: the
// credential blob is the document's ENTIRE content. Read fetches the whole
// document and parses/validates it as a v1 Blob; Write replaces the whole
// document — creating it the first time, editing it thereafter. The blob
// JSON always travels to `op` over STDIN, never as a command-line argument,
// so it never appears in a process listing, shell history, or audit log of
// argv.
type OPStore struct {
	Runner CommandRunner
	Vault  string // 1Password vault name or ID
	Title  string // document title used the first time Write creates the item

	mu      sync.Mutex
	item    string // 1Password item identifier used for get/edit; empty until known
	vaultID string // vault id CAPTURED from the last successful Write's response; empty until known
}

// NewOPStore constructs an OPStore. item is the 1Password item identifier
// (name or ID) to read/edit if the document already exists; leave it empty
// for a store whose first operation will be a Write that creates it.
func NewOPStore(runner CommandRunner, vault, title, item string) *OPStore {
	return &OPStore{Runner: runner, Vault: vault, Title: title, item: item}
}

// ItemID returns the 1Password item identifier currently known to the store
// (empty until a Write creates the item, or one was supplied at construction).
func (s *OPStore) ItemID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.item
}

// VaultID returns the 1Password vault id CAPTURED from the most recent
// successful Write's `op document create|edit --format json` response. This
// is the resolved id even when Vault was configured as a vault NAME rather
// than an id, so a caller (e.g. the Slack PKCE setup flow) can persist the
// exact vault the document lives in without a second lookup. Empty until a
// Write has succeeded.
func (s *OPStore) VaultID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vaultID
}

var errOPStoreNoItem = errors.New("slackoauth: OPStore has no item yet; nothing has been written")

// Read fetches the document via `op document get ITEM --vault VAULT` (no
// stdin) and parses/validates the result as a v1 Blob.
func (s *OPStore) Read(ctx context.Context) (Blob, error) {
	if s.Runner == nil {
		return Blob{}, errors.New("slackoauth: OPStore.Runner is required")
	}
	if s.Vault == "" {
		return Blob{}, errors.New("slackoauth: OPStore.Vault is required")
	}
	s.mu.Lock()
	item := s.item
	s.mu.Unlock()
	if item == "" {
		return Blob{}, errOPStoreNoItem
	}
	out, err := s.Runner.Run(ctx, nil, "op", "document", "get", item, "--vault", s.Vault)
	if err != nil {
		return Blob{}, fmt.Errorf("slackoauth: op document get: %w", err)
	}
	return ParseBlob(out)
}

// opDocumentMeta is the slice of `op document create|edit --format json`'s
// response we need: the item and vault identifiers it assigned/confirmed.
type opDocumentMeta struct {
	ID    string `json:"id"`
	Vault struct {
		ID string `json:"id"`
	} `json:"vault"`
}

// Write replaces the whole document with b: creates it the first time (`op
// document create - --vault VAULT --title TITLE --format json`) and edits it
// thereafter (`op document edit ITEM - --vault VAULT --format json`). The
// blob JSON travels over stdin only. b is validated before anything is sent.
func (s *OPStore) Write(ctx context.Context, b Blob) error {
	if s.Runner == nil {
		return errors.New("slackoauth: OPStore.Runner is required")
	}
	if s.Vault == "" {
		return errors.New("slackoauth: OPStore.Vault is required")
	}
	if err := b.Validate(); err != nil {
		return err
	}
	body, err := MarshalBlob(b)
	if err != nil {
		return err
	}

	s.mu.Lock()
	item := s.item
	s.mu.Unlock()

	var out []byte
	if item == "" {
		if s.Title == "" {
			return errors.New("slackoauth: OPStore.Title is required to create a new item")
		}
		out, err = s.Runner.Run(ctx, body, "op", "document", "create", "-", "--vault", s.Vault, "--title", s.Title, "--format", "json")
		if err != nil {
			return fmt.Errorf("slackoauth: op document create: %w", err)
		}
	} else {
		out, err = s.Runner.Run(ctx, body, "op", "document", "edit", item, "-", "--vault", s.Vault, "--format", "json")
		if err != nil {
			return fmt.Errorf("slackoauth: op document edit: %w", err)
		}
	}

	meta, metaErr := decodeOPDocumentMeta(out)
	if item == "" && (metaErr != nil || meta.ID == "") {
		// Some op CLI versions successfully create a stdin-backed Document but
		// return the document content, rather than item metadata, even with
		// --format json. Resolve the non-secret metadata by the unique title.
		// This second call carries no credential bytes on stdin or argv.
		lookupOut, lookupErr := s.Runner.Run(ctx, nil, "op", "item", "get", s.Title, "--vault", s.Vault, "--format", "json")
		if lookupErr != nil {
			return fmt.Errorf("slackoauth: op document was created but its item metadata could not be resolved: %w", lookupErr)
		}
		meta, metaErr = decodeOPDocumentMeta(lookupOut)
	}
	if metaErr != nil {
		return metaErr
	}
	if meta.ID == "" {
		// On edit the item identifier is already authoritative; op's response
		// shape is not allowed to make a successful durable rotation look like
		// a failed write. Creates must always resolve an assigned identifier.
		meta.ID = item
	}
	if meta.ID == "" {
		return errors.New("slackoauth: op document response is missing the item id")
	}
	if meta.Vault.ID == "" {
		meta.Vault.ID = s.Vault
	}
	s.mu.Lock()
	s.item = meta.ID
	s.vaultID = meta.Vault.ID
	s.mu.Unlock()
	return nil
}

func decodeOPDocumentMeta(out []byte) (opDocumentMeta, error) {
	var meta opDocumentMeta
	if err := json.Unmarshal(out, &meta); err != nil {
		return opDocumentMeta{}, fmt.Errorf("slackoauth: decode op document response: %w", err)
	}
	return meta, nil
}

// Delete archives (soft-deletes) the store's 1Password document via `op
// document delete ITEM --vault VAULT --archive`. This is what `pix slack
// disable`'s OAuth-mode path calls AFTER Slack itself has confirmed the
// credential is revoked (auth.revoke), so the document holding a now-dead
// token doesn't linger in 1Password either. Both arguments (item, vault) are
// non-secret identifiers, so this call needs no stdin and never puts a
// credential on argv. It refuses cleanly when no item is known yet (nothing
// has ever been written, so there is nothing to delete) rather than running
// a doomed command with an empty identifier.
func (s *OPStore) Delete(ctx context.Context) error {
	if s.Runner == nil {
		return errors.New("slackoauth: OPStore.Runner is required")
	}
	if s.Vault == "" {
		return errors.New("slackoauth: OPStore.Vault is required")
	}
	s.mu.Lock()
	item := s.item
	s.mu.Unlock()
	if item == "" {
		return errOPStoreNoItem
	}
	if _, err := s.Runner.Run(ctx, nil, "op", "document", "delete", item, "--vault", s.Vault, "--archive"); err != nil {
		return fmt.Errorf("slackoauth: op document delete: %w", err)
	}
	return nil
}
