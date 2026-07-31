package main

import (
	"errors"
	"fmt"
	"os"
)

// cleanupGeneratedKitDirs removes only the exact transient kit directories
// returned by the launcher synthesizers. Pack-authored kit paths are never
// passed here: they have a separate lifetime and ownership model.
func cleanupGeneratedKitDirs(paths []string) error {
	seen := map[string]bool{}
	var errs []error
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove generated kit %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// preflightBeforeReplace preserves the destructive launch ordering: every
// known hard-fail preflight completes before replacement may remove the old
// sandbox. Keeping the ordering in a small helper makes it directly testable.
func preflightBeforeReplace(preflight, replace func() error) error {
	if err := preflight(); err != nil {
		return err
	}
	return replace()
}
