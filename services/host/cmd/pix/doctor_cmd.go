// doctor_cmd.go — the argv seams for `pix doctor` and `pix status`. They own
// os.Exit and build the real env; the report itself is workflow/doctor.
package main

import (
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/readiness"
	"pix/host/workflow/doctor"
	"pix/host/workspace"
)

// runDoctorCmd is the CLI entry point wired into main's dispatch. Exit codes
// are part of the shared contract (snapshot.ExitCode): 0 = every core and
// requested axis is ready, 1 = a POSITIVELY VERIFIED core/requested failure
// (verdict todo/denied) or a config-load error, 2 = usage error, 3 = a
// core/requested axis could not be verified from here.
func runDoctorCmd(argv []string) {
	jsonOut, verbose, err := doctor.ParseDoctorArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(doctor.Usage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n\n%s", err, doctor.Usage)
		os.Exit(2)
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n", err)
		os.Exit(1)
	}
	r := doctor.RunDoctor(cfg, defaultShellEnv())
	r.Services = cfg.Services
	r.MCP = cfg.MCP
	if jsonOut {
		_ = cli.WriteJSONOut(os.Stdout, doctor.JsonView(r, ""))
	} else {
		r.Render(os.Stdout, verbose, doctor.Hints())
	}
	// The exit code is derived by the SHARED contract (snapshot.ExitCode):
	// 0 ready, 1 a verified core/requested failure, 3 core/requested axes
	// that could not be verified from here. Usage errors above already exit 2.
	// Doctor used to collapse 3 into 0; two exit contracts over one snapshot
	// would reintroduce exactly the disagreement this wave removes.
	if code := r.Snapshot().ExitCode(); code != readiness.ExitReady {
		os.Exit(code)
	}
}

// runStatusCmd is the `status` verb AND the bare-`pix` landing screen: a
// fast, read-only control panel answering "what state am I in, what's my next
// move" — WITHOUT launching anything. It replaces the old footgun where bare
// `pix` spun up a sandbox.
func runStatusCmd(argv []string) {
	jsonOut, err := doctor.ParseStatusArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(doctor.StatusUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix status: %v\n\n%s", err, doctor.StatusUsage)
		os.Exit(2)
	}
	cfg, name, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix status: %v\n", err)
		os.Exit(1)
	}
	// ONE exit contract (snapshot.ExitCode), with the 3 arm suppressed: status
	// is the landing screen and a JSON-scraping script must never fail merely
	// because a fact could not be checked from here (inside the sandbox, sbx is
	// absent and half the axes are unverifiable by construction). A POSITIVELY
	// verified core failure still exits 1, and the same integer is published as
	// the JSON `exit` sibling, so a reader of the rows and a reader of $? can
	// never disagree.
	if code := doctor.RenderStatus(cfg, name, defaultShellEnv(), os.Stdout, jsonOut); code != readiness.ExitReady {
		os.Exit(code)
	}
}
