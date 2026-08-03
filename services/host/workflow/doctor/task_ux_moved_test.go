// Moved from cmd/pix: the subject is a doctor internal.
package doctor

import (
	"os"
	"path/filepath"
	"pix/host/workspace"
	"strings"
	"testing"
)

func TestTaskStateSummary(t *testing.T) {
	state := t.TempDir()
	data := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", data)
	// Two task metas across one repo dir.
	metaDir := filepath.Join(workspace.TaskStateRoot(), "proj-abcd1234", "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(metaDir, "a.json"), "{}\n")
	writeFile(t, filepath.Join(metaDir, "b.json"), "{}\n")
	// An artifact file to size.
	artDir := filepath.Join(workspace.TaskArtifactRoot(), "proj-abcd1234", "a", "ts")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(artDir, "doc.md"), strings.Repeat("x", 100))

	tasks, bytes := taskStateSummary()
	if tasks != 2 {
		t.Errorf("tasks = %d, want 2", tasks)
	}
	if bytes < 100 {
		t.Errorf("artifact bytes = %d, want >= 100", bytes)
	}
}

// --- Story 4: uninstall --purge-data ----------------------------------------

func TestStatusRender_ArtifactsWithoutTasks(t *testing.T) {
	var sb strings.Builder
	statusReport{Version: "v", Tasks: 0, ArtifactB: 4096}.render(&sb)
	if !strings.Contains(sb.String(), "4.0KB artifacts") {
		t.Errorf("artifact-only status did not render the tasks line:\n%s", sb.String())
	}
}

// --- review loop 2 hardening ------------------------------------------------
