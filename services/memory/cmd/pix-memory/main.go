// pix-memory is the standalone memory MCP service
// (docs/design/pix-v2-architecture.md §9): a Streamable HTTP /mcp endpoint
// plus /healthz, backed by one sqlite store mounted at /data.
//
// Env: MEMORY_PORT (8080), MEMORY_BIND (0.0.0.0), MEMORY_DB
// (<MEMORY_DATA_DIR>/memory.db), MEMORY_DATA_DIR (/data), OLLAMA_HOST,
// MEMORY_EMBED_MODEL, MEMORY_WATCHER_MODEL, MEMORY_CAPTURE_MODE.
//
// Auth (security re-review round 1 blocker #1): the bearer token `pix
// setup` generates on the host is read from a FILE — MEMORY_AUTH_TOKEN_FILE
// (default defaultAuthTokenFile, "/run/secrets/pix-memory-auth"), bind-
// mounted read-only by `docker create -v <host-path>:<mount-path>:ro` —
// never from `docker create --env-file`/`-e`, both of which would land the
// secret in this container's own Config.Env, which `docker inspect` exposes
// in full to anything on the host with inspect access (see
// pix/host/container.AuthTokenMountPath and .EnsureMemoryAuthToken). The
// MEMORY_AUTH_TOKEN env var remains an OPTIONAL DEV FALLBACK, consulted only
// when the token file is absent or empty — e.g. `go run ./cmd/pix-memory`
// outside any container — and production never sets it. /mcp refuses every
// request when no token resolves from either source; /healthz never
// requires one (loopback-only, carries no memory content).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pix-memory/server"
	"pix-memory/store"
)

// defaultAuthTokenFile is the fixed in-container mount path pix-memory reads
// its bearer token from — the exact path container.AuthTokenMountPath names
// on the host side of the same bind mount.
const defaultAuthTokenFile = "/run/secrets/pix-memory-auth"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveAuthToken reads the bearer token from its mounted FILE first — the
// only path production ever wires — and falls back to the MEMORY_AUTH_TOKEN
// env var only when that file is absent, unreadable, or empty (the dev-only
// shape: no container, no mount). A file that exists but is merely empty is
// treated the same as absent, not as "deliberately no auth".
func resolveAuthToken() string {
	path := envOr("MEMORY_AUTH_TOKEN_FILE", defaultAuthTokenFile)
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok
		}
	}
	return os.Getenv("MEMORY_AUTH_TOKEN")
}

func main() {
	dbPath := store.DBPath()
	st, err := store.Open(dbPath, store.Embed)
	if err != nil {
		log.Fatalf("pix-memory: open store at %s: %v", dbPath, err)
	}
	defer st.Close()

	authToken := resolveAuthToken()
	if authToken == "" {
		log.Printf("pix-memory: WARNING: no auth token resolved from %s or MEMORY_AUTH_TOKEN; /mcp will refuse every request (/healthz still answers)", envOr("MEMORY_AUTH_TOKEN_FILE", defaultAuthTokenFile))
	}
	mux := server.NewMux(st, authToken)
	addr := envOr("MEMORY_BIND", "0.0.0.0") + ":" + envOr("MEMORY_PORT", "8080")
	httpServer := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("pix-memory %s: /mcp and /healthz on http://%s (db %s)", server.Version, addr, dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("pix-memory: listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("pix-memory: shutdown: %v", err)
	}
}
