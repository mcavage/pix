package uat

import (
	"os"
	"path/filepath"
	"testing"
)

type mockGitVerifier struct{}

func (m *mockGitVerifier) FileExistsAtCommit(repoPath, commitSHA, filePath string) (bool, error) {
	return true, nil
}

func TestValidateScenarioPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "uat-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	root := filepath.Join(tmpDir, "root")
	os.Mkdir(root, 0755)

	// Create a file outside root
	outsideFile := filepath.Join(tmpDir, "outside.yaml")
	os.WriteFile(outsideFile, []byte("test"), 0644)

	// Create a symlink in root pointing outside
	symlink := filepath.Join(root, "symlink.yaml")
	os.Symlink(outsideFile, symlink)

	// Create valid scenario file
	scenarioFile := filepath.Join(root, "scenario.yaml")
	os.WriteFile(scenarioFile, []byte("test"), 0644)

	verifier := &mockGitVerifier{}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid", "scenario.yaml", false},
		{"absolute", "/etc/passwd", true},
		{"traversal", "../scenario.yaml", true},
		{"symlink-escape", "symlink.yaml", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateScenarioPath(root, tt.path, verifier, "repo", "sha")
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateScenarioPath() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
