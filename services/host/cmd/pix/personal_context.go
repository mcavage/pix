package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
)

// synthesizePersonalContextKit turns the user's durable AGENTS.md into the sbx
// agentInstructions layer. It is appended after pack kits, so personal instructions
// compose above organizational context while workspace AGENTS.md remains the
// most specific layer. Skills are mounted live separately by buildSbxArgs.
func synthesizePersonalContextKit() (string, error) {
	source := filepath.Join(config.ContextDir(), "AGENTS.md")
	b, err := os.ReadFile(source)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", nil
	}
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(state, "context-kits")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "personal-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	var spec strings.Builder
	spec.WriteString("schemaVersion: \"2\"\nkind: mixin\nname: pix-personal-context\nagentInstructions:\n  content: |\n")
	for _, line := range strings.Split(string(b), "\n") {
		fmt.Fprintf(&spec, "    %s\n", line)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec.String()), 0o600); err != nil {
		return "", err
	}
	complete = true
	return dir, nil
}

func readPersonalInstructions() (string, error) {
	b, err := os.ReadFile(filepath.Join(config.ContextDir(), "AGENTS.md"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func personalSkillsDir() string {
	dir := filepath.Join(config.ContextDir(), "skills")
	if dirHasEntries(dir) {
		return dir
	}
	return ""
}
