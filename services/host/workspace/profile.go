package workspace

import (
	"os"
	"strings"

	"pix/host/config"
)

// LoadResolvedConfig loads the config AND the active workspace's memory scope:
// <cwd>/.pix/profile, the marker packinfo.WriteMemoryScope writes on `pack use`/
// `pix run` (see pack.go). "Workspace profiles" as a standalone config concept
// were removed, but the memory-scope FILE that happens to be named "profile"
// on disk was not — a call site (`pix memory`) still needs the real scope to
// forward to the daemon, or every pack-scoped memory silently reads/writes the
// default shared bucket instead of its own. Returning "" unconditionally here
// was that bug.
func LoadResolvedConfig() (*config.Config, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, "", err
	}
	return cfg, ReadProfileScope(), nil
}

// ReadProfileScope reads the memory scope for the CURRENT directory (the same
// cwd `pix run`/`os.Getwd` callers already treat as "the active workspace";
// see doctorOptions in cmd/pix). Fail-safe like its writer: an unresolvable
// cwd, a missing marker, or a symlinked .pix (ReadStateFile's guard) all yield
// the unscoped "" rather than an error that would abort an unrelated command.
func ReadProfileScope() string {
	ws, err := os.Getwd()
	if err != nil {
		return ""
	}
	b, err := ReadStateFile(ws, "profile")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
