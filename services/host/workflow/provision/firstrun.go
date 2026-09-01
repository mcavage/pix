package provision

import (
	"os"

	"pix/host/config"
)

// FirstRunNeeded reports whether this PIX_HOME has no machine configuration
// yet. It is read-only. The interactive bare-pix entry point uses this to run
// the real setup command before its first launch; explicit `pix run` remains
// the caller's opt-out, and a non-interactive bare invocation remains read-only.
func FirstRunNeeded() bool {
	_, err := os.Stat(config.Path())
	return os.IsNotExist(err)
}
