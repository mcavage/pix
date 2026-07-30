package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// validateCreateKits asks the installed sbx parser to validate local kits
// before `sbx run` performs any interactive/global setup. This catches
// nightly schema skew without opening a policy wizard and ending in a raw
// YAML decoder dump. Remote kits remain sbx's responsibility because
// validating them here would add an eager network fetch.
func validateCreateKits(args []string, validate func(string) (string, error)) error {
	if validate == nil {
		return nil
	}
	for _, ref := range localKitArgs(args) {
		out, err := validate(ref)
		if err == nil {
			continue
		}
		detail := firstOutputLine(out)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("this Pix kit does not match the installed Docker Sandboxes schema (%s)\n  detail: %s\n  fix: update Pix and Docker Sandboxes, then retry", ref, detail)
	}
	return nil
}

func localKitArgs(args []string) []string {
	var refs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--kit" {
			continue
		}
		ref := args[i+1]
		i++
		if filepath.IsAbs(ref) || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
			refs = append(refs, ref)
		}
	}
	return refs
}

func firstOutputLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func validateSbxKit(ref string) (string, error) {
	b, err := exec.Command("sbx", "kit", "validate", ref).CombinedOutput()
	return string(b), err
}

func validateSetupKit(version string, repoRoot func() (string, error), validate func(string) (string, error)) error {
	if isReleased(version) || repoRoot == nil {
		return nil
	}
	root, err := repoRoot()
	if err != nil {
		return nil // run will use the remote main kit
	}
	return validateCreateKits([]string{"--kit", filepath.Join(root, "pi-kit")}, validate)
}
