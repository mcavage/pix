package sys

import "os"

// Three predicates that were written three times in cmd/pix under two names
// that did not say which was which: fileExists meant "a regular file",
// fileExistsTask meant "anything at this path". They differ on a directory,

// IsRegularFile reports whether path exists and is a regular file (a directory
// is NOT a regular file).
func IsRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// PathExists reports whether anything exists at path, file or directory.
func PathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DirHasEntries reports whether path is a directory with at least one entry.
func DirHasEntries(path string) bool {
	ents, err := os.ReadDir(path)
	return err == nil && len(ents) > 0
}
