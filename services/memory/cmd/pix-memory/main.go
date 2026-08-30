// pix-memory is the standalone memory MCP service
// (docs/design/pix-v2-architecture.md §9): a Streamable HTTP /mcp endpoint
// plus /healthz, backed by one sqlite store mounted at /data.
//
// Env: MEMORY_PORT (8080), MEMORY_BIND (0.0.0.0), MEMORY_DB
// (<MEMORY_DATA_DIR>/memory.db), MEMORY_DATA_DIR (/data), OLLAMA_HOST,
// MEMORY_EMBED_MODEL, MEMORY_WATCHER_MODEL, MEMORY_CAPTURE_MODE.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pix-memory/server"
	"pix-memory/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dbPath := store.DBPath()
	st, err := store.Open(dbPath, store.Embed)
	if err != nil {
		log.Fatalf("pix-memory: open store at %s: %v", dbPath, err)
	}
	defer st.Close()

	mux := server.NewMux(st)
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
