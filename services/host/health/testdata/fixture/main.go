// fixture is a REAL executable used by the health probe tests. It is not a
// mock and it is not a fake seam: the tests build this program, put it on a
// probe's argv, and let the probe exec it for real — same code path, same
// context deadline, same exit-status classification as production.
//
// One binary plays every failure mode a host probe must survive, chosen by
// argv[1]:
//
//	healthy    exit 0 with a well-formed version banner
//	notloaded  exit 1 with the "could not find service" text launchctl prints
//	           for an agent that is genuinely not loaded (a POSITIVE absence)
//	denied     exit 1 with an explicit policy refusal
//	broken     exit 1 with a generic error (we do NOT know what is wrong)
//	crash      die on SIGKILL — no exit status, no output
//	hang       sleep past any sane probe deadline, ignoring SIGTERM
//	malformed  exit 0 with output that carries no recognizable version
//	keys       exit 0 listing every model provider key, the way a key store
//	           that ANSWERED does
//	nokeys     exit 0 listing a store that answered and holds no model key
//	mcpls      exit 0 listing slack + notion as registered MCP servers
//	mcpnone    exit 0 with an MCP listing that holds nothing
//	authok     exit 0 saying the named server is authenticated
//	authno     exit 1 saying the named server is NOT authenticated (a
//	           POSITIVE answer, even though the process failed)
//	authnoise  exit 1 with output that says nothing about auth either way
//
// The distinction the tests exist to pin: only `notloaded` (and a missing
// binary) may render absent. broken/crash/hang/malformed are all unknown,
// because the probe did not learn anything.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// argAt returns os.Args[i] or "" — the auth modes are handed the server name
// as the argument after the mode.
func argAt(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}

func main() {
	mode := argAt(1)
	switch mode {
	case "healthy":
		fmt.Println("pix-fixture version 9.9.9")
	case "notloaded":
		fmt.Fprintln(os.Stderr, "Could not find service \"com.pix.serve\" in domain for user")
		os.Exit(1)
	case "denied":
		fmt.Fprintln(os.Stderr, "operation not permitted: blocked by organization policy")
		os.Exit(1)
	case "broken":
		fmt.Fprintln(os.Stderr, "fixture: something went wrong")
		os.Exit(1)
	case "crash":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
	case "hang":
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		time.Sleep(time.Minute)
	case "keys":
		fmt.Println("NAME        SCOPE\nanthropic   global\nopenai      global\ngoogle      global")
	case "nokeys":
		fmt.Println("NAME        SCOPE\ngithub      global")
	case "mcpls":
		fmt.Println("NAME     TRANSPORT\nslack    stdio\nnotion   remote")
	case "mcpnone":
		fmt.Println("NAME     TRANSPORT")
	case "authok":
		fmt.Printf("%s: authenticated\n", argAt(2))
	case "authno":
		fmt.Fprintf(os.Stderr, "%s: not authenticated\n", argAt(2))
		os.Exit(1)
	case "authnoise":
		fmt.Fprintln(os.Stderr, "fixture: the control plane fell over")
		os.Exit(1)
	case "malformed":
		fmt.Println("\x00\x01 not a version banner at all")
	default:
		fmt.Fprintf(os.Stderr, "fixture: unknown mode %q\n", mode)
		os.Exit(64)
	}
}
