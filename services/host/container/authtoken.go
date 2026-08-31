// authtoken.go — the pix-memory bearer credential (security re-review HIGH
// finding: "unauthenticated memory container exposed on host loopback").
// A random token is generated once (by `pix setup`, via EnsureMemoryAuthToken)
// and persisted under PIX_HOME state, mode 0600, so `pix-memory`'s /mcp
// endpoint can require it (see services/memory/server's requireAuth) and
// every Pix caller that needs to REACH /mcp (a real launch's built-in MCP
// declaration, `pix env --effective`'s preview) can read the SAME value back
// without ever regenerating or duplicating it.
//
// This lives in package container — L1 capability, the lowest layer both
// cmd/pix (L4) and workflow/env (L3) may import — rather than in
// workflow/provision (L3, a WORKFLOW package): workflow/env is barred from
// importing a sibling workflow/* package (its own doc comment), so the token
// helpers and container.Spec have to share one importable-by-both home.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"pix/host/pixhome"
)

// MemoryAuthTokenEnvKey is the DEV-ONLY fallback env var name
// services/memory/cmd/pix-memory/main.go still accepts when its mounted
// token FILE (AuthTokenMountPath) is absent — e.g. `go run ./cmd/pix-memory`
// outside any container. Production `pix setup`/Reconcile never sets this
// as a literal `-e`/`--env-file` value; see container.Spec.AuthTokenFile's
// doc for why (security re-review round 1 blocker #1: that would land the
// secret in the container's own Config.Env, which `docker inspect` exposes
// in full).
const MemoryAuthTokenEnvKey = "MEMORY_AUTH_TOKEN"

// memoryAuthTokenFileName is the token file's name under home.StateMemory —
// beside (never inside) the sqlite data the container itself owns. It holds
// the RAW token only (no "KEY=value" wrapping): this file is bind-mounted
// read-only straight into the container at container.AuthTokenMountPath, so
// its on-host format is exactly what pix-memory reads back on the other end
// of that mount.
const memoryAuthTokenFileName = "auth.token"

// MemoryAuthTokenPath is <home>/state/memory/auth.token: the one file both
// the token generator (EnsureMemoryAuthToken) and the container's read-only
// bind mount (homeContainerSpec's AuthTokenFile) agree names the same
// secret.
func MemoryAuthTokenPath(home pixhome.Paths) string {
	return filepath.Join(home.StateMemory, memoryAuthTokenFileName)
}

// EnsureMemoryAuthToken returns the persisted pix-memory bearer token,
// generating and durably writing a fresh 256-bit one (mode 0600) on first
// use. It is IDEMPOTENT: a later call returns the SAME token, so an
// already-created container's baked-in MEMORY_AUTH_TOKEN keeps matching the
// registered MCP URL/env-file across `pix setup` reruns — regenerating on
// every call would silently desync a running container (whose env is fixed
// at creation) from a freshly registered URL. Only `pix setup` calls this;
// every other reader uses ReadMemoryAuthToken (no generation) so a read-only
// command can never mutate host state (safety invariant 12).
func EnsureMemoryAuthToken(home pixhome.Paths) (string, error) {
	if tok, err := ReadMemoryAuthToken(home); err == nil && tok != "" {
		return tok, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pix-memory auth token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(home.StateMemory, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", home.StateMemory, err)
	}
	content := tok + "\n"
	path := MemoryAuthTokenPath(home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("install %s: %w", path, err)
	}
	return tok, nil
}

// ReadMemoryAuthToken reads the persisted token WITHOUT ever generating one
// — the read-only half doctor/run/env-preview callers use. A missing file
// or an unparsable one returns ("", nil): "no token yet" is not a failure,
// it is the pre-`pix setup` state every builtin-MCP caller already treats
// as "omit that built-in" (see cmd/pix/run_env.go, workflow/env/effective.go).
func ReadMemoryAuthToken(home pixhome.Paths) (string, error) {
	data, err := os.ReadFile(MemoryAuthTokenPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// MemoryMCPURL renders the loopback MCP endpoint a reconciled pix-memory
// container publishes. When token is non-empty it is embedded as a
// "?token=" query parameter — the loopback-URL credential fallback
// (security re-review HIGH finding) a native sbx MCP server declaration
// must use in place of a header: envinfo.MCPServer (the native
// `.sbxenv.yaml`/effective-document schema) and `sbx mcp add` both carry
// only name/url/command/args, with no field for a custom HTTP header, so a
// header-bearing declaration cannot be expressed at all. pix-memory's own
// requireAuth (services/memory/server/server.go) accepts this query
// parameter as equivalent to "Authorization: Bearer <token>" for exactly
// this reason. An empty token renders the bare URL (the pre-`pix setup`
// state, or a caller that could not resolve one) — the built-in's own
// degrade-to-omitted callers already treat a bare/absent URL as "not ready
// yet" rather than a broken one.
func MemoryMCPURL(spec Spec, token string) string {
	u := fmt.Sprintf("http://127.0.0.1:%d/mcp", spec.HostPort)
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u
}
