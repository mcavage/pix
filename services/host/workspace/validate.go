package workspace

import (
	"fmt"
	"os"
)

// ErrNotDirectory is returned by Validate when the positional workspace
// argument does not name a directory. It is a sentinel so the CALLER can add
// the "did you mean `pix <verb>`?" hint — only cmd/pix knows the verb table,
type ErrNotDirectory struct{ Path string }

func (e ErrNotDirectory) Error() string { return fmt.Sprintf("%q is not a directory", e.Path) }

// Validate reports whether ws is usable as a run workspace. "." is always
// valid; anything else must be an existing directory.
func Validate(ws string) error {
	if ws == "." {
		return nil
	}
	if fi, err := os.Stat(ws); err == nil && fi.IsDir() {
		return nil
	}
	return ErrNotDirectory{ws}
}
