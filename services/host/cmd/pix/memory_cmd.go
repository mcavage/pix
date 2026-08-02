package main

// memory_cmd.go is the memory verb's composition root, which is the ONLY thing
// that could not live in the memory package.
//
// arch_test.go rejected it there: choosing to auto-start the daemon reaches the
// service capability, and resolving config reaches workspace, so memory was a
// capability importing two siblings. memory.RunCore was already parameterised
// over both; only the wrapper that PICKS them belongs up here, where composing
// capabilities is the job.

import (
	"os"

	"pix/host/cli"
	"pix/host/memory"
	"pix/host/rpc"
	"pix/host/service"
	"pix/host/workspace"
)

// Run is the `memory` verb tree — the host-side CLI over the memory daemon
// (:11435), so you can inspect and repair the agent's recall WITHOUT launching a
// sandbox:
//
//	pix memory recall <query> [--limit N] [--project P] [--json]
//	pix memory remember <text...> [--json]
//	pix memory forget <id>
//	pix memory learnings [--min N] [--json]   (recurring learnings)
//	pix memory stats [--json]
//
// Every verb degrades cleanly when the daemon is down: an actionable message on
// stderr + exit code 3 (rpc.ExitServiceDown), distinct from usage (2) / generic (1).
func runMemory(argv []string) {
	ctx := "memory"
	if len(argv) > 0 && argv[0] != "-h" && argv[0] != "--help" {
		ctx = "memory " + argv[0]
	}
	// Lazy auto-start: if the memory daemon is down, spin it up detached before
	// dispatching, so the common "daemon just not up yet" case just works. Never
	// on a help request (help must stay side-effect free); best-effort — on
	// failure the rpc.ErrServiceDown path below still degrades with exit 3.
	if len(argv) > 0 && !cli.WantsHelp(argv) {
		service.EnsureUp([]string{"memory"}, service.EnsureTimeout)
	}
	if err := memory.RunCore(argv, workspace.LoadResolvedConfig, rpc.MemoryClient, os.Stdout); err != nil {
		cli.ExitFromErr(ctx, err)
	}
}
