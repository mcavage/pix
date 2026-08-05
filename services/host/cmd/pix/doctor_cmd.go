// doctor_cmd.go — the argv seams for `pix doctor` and `pix status`. They own
// os.Exit and load the config; the probing and the report are workflow/doctor.
package main

import (
	"context"
	"fmt"
	"os"

	"pix/host/cli"
	"pix/host/health"
	"pix/host/workflow/doctor"
	"pix/host/workspace"
)

// runDoctorCmd is the CLI entry point wired into main's dispatch. The exit
// contract is health's: 0 when nothing REQUIRED verified a gap (a check that
// could not be made from here does NOT fail the process), 1 when a required
// check verified one or the config failed to load, 2 for a usage error.
func runDoctorCmd(argv []string) {
	jsonOut, verbose, err := doctor.ParseDoctorArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(doctor.Usage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n\n%s", err, doctor.Usage)
		os.Exit(health.ExitUsage)
	}
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix doctor: %v\n", err)
		os.Exit(health.ExitNotReady)
	}
	if code := doctor.RunDoctor(context.Background(), cfg, profile, os.Stdout, doctor.Options{}, jsonOut, verbose); code != health.ExitOK {
		os.Exit(code)
	}
}

// runStatusCmd is the `status` verb AND the bare-`pix` landing screen: a
// fast, read-only glance answering "what state am I in, what's my next move"
// — WITHOUT launching anything. It replaces the old footgun where bare `pix`
// spun up a sandbox.
//
// It exits 0 whatever the verdict (doctor.StatusExit). The landing screen is
// what runs under `set -e` and in prompts; a probe that could not see
// something must not take a shell down with it, and the verdict is one
// `pix doctor` (or the `exit` field of --json) away.
func runStatusCmd(argv []string) {
	jsonOut, err := doctor.ParseStatusArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(doctor.StatusUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix status: %v\n\n%s", err, doctor.StatusUsage)
		os.Exit(health.ExitUsage)
	}
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix status: %v\n", err)
		os.Exit(health.ExitNotReady)
	}
	doctor.RenderStatus(context.Background(), cfg, profile, os.Stdout, doctor.Options{}, jsonOut)
}
