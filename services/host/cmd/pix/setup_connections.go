package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/pixhome"
	"pix/host/secret"
)

// Basic environments offer optional personal connections, independently of
// their main model. Environments owning inference/auth keep their own flow.
func setupOptionalConnections(d *cli.Deps, home pixhome.Paths, env hostenv.Env, name string) error {
	if !d.Interactive {
		return nil
	}
	sidecar, err := envinfo.ParseSidecar(filepath.Join(home.EnvironmentDir(name), "pix.toml"))
	if err != nil {
		return err
	}
	if len(sidecar.Inference.Backends) > 0 || len(sidecar.Inference.Models) > 0 || sidecar.Models.Exclusive || len(sidecar.Setup) > 0 {
		return nil
	}
	content, err := env.ReadFile(secret.DefaultOpRefsPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read account references: %w", err)
	}
	saved := map[string]bool{}
	for _, ref := range secret.ParseOpRefs(content, nil) {
		saved[ref.Key] = ref.IsRef && !ref.Placeholder
	}
	keys := append(append([]secret.ProviderKeyRef{}, secret.ProviderKeyRefOrder...), secret.ToolKeyRefOrder...)
	keys = append(keys, secret.GitHubKeyRef)
	printed := false
	for _, key := range keys {
		if saved[key.EnvVar] {
			continue
		}
		if !printed {
			fmt.Fprintln(d.Out, "Optional connections for model switching, subagents, web search, and GitHub.")
			fmt.Fprintln(d.Out, "Paste a 1Password secret reference for each, or press Enter to skip.")
			printed = true
		}
		label := providerDisplayName(key.Name)
		if key.Name == "github" {
			label = "GitHub"
		}
		if key.Name == "parallel" {
			label = "Parallel web search"
		}
		_, _ = d.Ask(cli.Question{Label: label, Example: "op://Private/Service/api-key", Accept: func(value string) error {
			ref := secret.NormalizeOpRef(value)
			if !strings.HasPrefix(ref, "op://") || strings.ContainsAny(ref, "\r\n") || secret.HasPlaceholder(ref) {
				return fmt.Errorf("enter a 1Password secret reference, or press Enter to skip")
			}
			if !secret.OpInstalled(env) {
				return fmt.Errorf("install the 1Password CLI to connect this account, or press Enter to skip")
			}
			fmt.Fprintln(d.Out, "Checking access to the 1Password reference…")
			value, timedOut, err := env.RunWithin(30*time.Second, "op", "read", ref)
			if err != nil || timedOut || strings.TrimSpace(value) == "" {
				return fmt.Errorf("could not read that reference from 1Password; check access or press Enter to skip")
			}
			return secret.WriteOpRefQuiet(env, key.EnvVar, ref)
		}})
	}
	return nil
}
