package main

import "fmt"

// noHostResolver makes the local-MCP set report as UNKNOWN, so a test can
// assert the unknown-set behaviour without a pix-host on disk. A copy of
// onboard's own: it is test-only, so exporting it would put scaffolding in that
// package's public API.
func noHostResolver() (string, error) { return "", fmt.Errorf("no host binary in test") }
