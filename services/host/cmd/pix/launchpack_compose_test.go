// launchpack_compose_test.go — the composition run_cmd.go performs for a pack at
// launch, in one place the pack-at-launch cases can call.
//
// It is the composition root's job, and it is TWO steps on purpose: workflow/
// pack resolves and TRUST-VERIFIES the active stack into a
// packinfo.LaunchContribution, then workflow/launch folds that value into the
// launch options. Neither workflow may call the other, so this file is where the
// tests exercise the same two calls run_cmd.go makes.
package main

import (
	"io"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

func packApplyForTest(cfg *config.Config, o *launch.RunOpts, env hostenv.Env, warn io.Writer) (string, error) {
	contributed, err := pack.ResolveLaunchContribution(cfg, o.Pack, env, warn)
	if err != nil {
		return "", err
	}
	return o.ApplyPackContribution(contributed), nil
}
