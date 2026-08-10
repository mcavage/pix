package sys

import "os"

// IsRegularFile reports whether path exists and is a regular file (a directory
// is NOT a regular file).
func IsRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// DirHasEntries reports whether path is a directory with at least one entry.
func DirHasEntries(path string) bool {
	ents, err := os.ReadDir(path)
	return err == nil && len(ents) > 0
}
