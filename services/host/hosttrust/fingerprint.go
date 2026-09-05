package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CanonicalDoc is a canonical-JSON-encoded document, ready to fingerprint.
// Its only field is unexported, so the sole way another package can produce
// one is Canonicalize — a caller cannot construct a CanonicalDoc by hand from
// an arbitrary struct, map, or byte slice, and Fingerprint's parameter type
// (CanonicalDoc, not any) then carries that same discipline all the way to
// the call site: handing it anything else is a COMPILE ERROR, not a
// convention the caller has to remember. See fingerprint_api_test.go for the
// negative-compile proof and the signature pin.
type CanonicalDoc struct {
	enc []byte
}

// Canonicalize marshals v — the caller's own canonical value: fixed field
// order via json tags, sorted slices, symlink-refused content hashes already
// folded in — into a CanonicalDoc. This is exactly the encoding step
// Fingerprint used to run inline (json.Marshal, nothing else added or
// reordered), so every existing caller that routes through Canonicalize then
// Fingerprint gets a byte-identical fingerprint to before.
func Canonicalize(v any) (CanonicalDoc, error) {
	enc, err := json.Marshal(v)
	if err != nil {
		return CanonicalDoc{}, err
	}
	return CanonicalDoc{enc: enc}, nil
}

// Fingerprint hashes doc's canonical JSON encoding: sha256, hex encoded. This
// is the generic engine behind every host-exec fingerprint in pix. It used to
// accept `any` and marshal it inline, so "hand me an already-canonical
// value" was a comment, not a compiler check — any Go value at all
// type-checked. Accepting only a CanonicalDoc, producible solely via
// Canonicalize, makes the same encoding INJECTIVE (a structured JSON
// document, not an ad-hoc delimiter-joined string a value could forge) *and*
// makes "canonicalize first" a type-level requirement instead of a
// convention.
func Fingerprint(doc CanonicalDoc) (string, error) {
	sum := sha256.Sum256(doc.enc)
	return hex.EncodeToString(sum[:]), nil
}
