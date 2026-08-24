package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint hashes v's CANONICAL JSON ENCODING: marshal, then sha256, hex
// encoded. This is the generic engine behind every host-exec fingerprint in
// pix — the caller is responsible for handing it an ALREADY-CANONICAL value
// (fixed field order via json tags, sorted slices, symlink-refused content
// hashes already folded in), because the encoding is what makes the hash
// INJECTIVE: an ad-hoc delimiter-joined string is not, since a value
// containing the delimiter could encode a DIFFERENT surface with an
// identical hash. The error is returned unwrapped so a caller can frame it in
// its own domain language (e.g. "encoding host-exec surface: %v").
func Fingerprint(v any) (string, error) {
	enc, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), nil
}
