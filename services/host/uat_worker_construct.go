package main

import (
	"fmt"
	"os"

	wuat "pix/host/workflow/uat"
)

// uatWorkerComponents bundles the one long-lived Runner a pix-host uat-worker
// process serves for its whole lifetime, plus what each per-connection
// workflow/uat.MCPServer needs alongside it. Extracted here — rather than
// re-inlined — because this construction used to live directly in the
// pre-split uat-mcp gateway; uat-worker is now the ONLY caller, and the
// gateway (uat_mcp.go) must never regain it (see TestUatMcpGatewayIsADumbRelay
// and docs/design/self-development-uat.md).
type uatWorkerComponents struct {
	runner         *wuat.Runner
	browserFactory wuat.BrowserFactory
	retryReport    map[string]string
}

// buildUatWorkerComponents wires the real Git/Exec/Sandbox/MCP/Image/Lease
// adapters used by production UAT runs against repo (the canonical checkout)
// and state (the session's runner state directory), then runs the one-time
// cleanup retry pass a fresh Runner performs at startup.
func buildUatWorkerComponents(repo, state string) (*uatWorkerComponents, error) {
	execAdapter := wuat.NewRealExec()
	gitAdapter := wuat.NewRealGit(repo, execAdapter)
	sandboxAdapter := wuat.NewRealSandbox(execAdapter)

	hostBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("uat-worker: failed to get executable path: %w", err)
	}

	browserFactory := wuat.NewRealBrowserFactory()
	mcpAdapter := wuat.NewRealMCP(hostBin, state, execAdapter, browserFactory)
	imageAdapter := wuat.NewRealImage(execAdapter)
	leaseAdapter := wuat.NewRealLease(state, execAdapter)

	runner, err := wuat.NewRunner(hostBin, repo, state, gitAdapter, execAdapter, sandboxAdapter, mcpAdapter, imageAdapter, leaseAdapter, 1)
	if err != nil {
		return nil, fmt.Errorf("uat-worker: failed to initialize runner: %w", err)
	}
	retryReport := runner.RetryCleanups()

	return &uatWorkerComponents{
		runner:         runner,
		browserFactory: browserFactory,
		retryReport:    retryReport,
	}, nil
}
