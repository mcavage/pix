package main

import (
	"pix/host/cli"
	"pix/host/workflow/launch"
)

// resumeCmd gives the host a first-class spelling for pi's exact session
// selector. The image rewrites pi's shutdown hint to this command, including
// both the session id and workspace, so it never falls back to -c's unrelated
// "newest session" semantics.
type resumeCmd struct {
	Session string `arg:"" help:"Exact pi session id from the exit hint." placeholder:"SESSION"`
	Dir     string `arg:"" optional:"" default:"." help:"Workspace containing .pi-sessions (default: current directory)." placeholder:"DIR"`
}

func (c *resumeCmd) opts() (launch.RunOpts, error) {
	o := launch.RunOpts{
		Workspace:   c.Dir,
		Passthrough: []string{"--session", c.Session},
	}
	if err := launch.ValidateRunWorkspace(o.Workspace, knownVerb); err != nil {
		return o, cli.UsageError{Err: err}
	}
	return o, nil
}

func (c *resumeCmd) Run(d *cli.Deps) error {
	o, err := c.opts()
	if err != nil {
		return err
	}
	return runLaunch(d, o)
}
