package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"pix/host/cli"
	"pix/host/config"
	workflowUat "pix/host/workflow/uat"
)

func (c *uatCmd) Help() string { return "Self-UAT and browser bootstrap." }

type uatCmd struct {
	Status  uatStatusCmd  `cmd:"" help:"Report UAT prerequisites and state."`
	Browser uatBrowserCmd `cmd:"" help:"Browser commands."`
}

type uatStatusCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

type UatStatus struct {
	SbxPresent      bool     `json:"sbx_present"`
	DockerPresent   bool     `json:"docker_present"`
	ChromePresent   bool     `json:"chrome_present"`
	ProfilePresent  bool     `json:"profile_present"`
	StaleUatRecords []string `json:"stale_uat_records"`
}

func (c *uatStatusCmd) Run(d *cli.Deps) error {
	status := UatStatus{
		StaleUatRecords: make([]string, 0),
	}

	if _, err := exec.LookPath("sbx"); err == nil {
		status.SbxPresent = true
	}
	if _, err := exec.LookPath("docker"); err == nil {
		status.DockerPresent = true
	}

	if _, err := exec.LookPath("google-chrome"); err == nil {
		status.ChromePresent = true
	} else if _, err := exec.LookPath("google-chrome-stable"); err == nil {
		status.ChromePresent = true
	} else if _, err := exec.LookPath("chromium"); err == nil {
		status.ChromePresent = true
	} else if _, err := exec.LookPath("chromium-browser"); err == nil {
		status.ChromePresent = true
	} else if _, err := exec.LookPath("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); err == nil {
		status.ChromePresent = true
	}

	if p, err := workflowUat.PeekProfilePath(); err == nil {
		if info, err := os.Stat(p); err == nil && info.IsDir() && info.Mode().Perm() == 0700 {
			status.ProfilePresent = true
		}
	}

	if stateDir, err := config.StateDir(); err == nil {
		uatDir := filepath.Join(stateDir, "uat")
		entries, err := os.ReadDir(uatDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
					status.StaleUatRecords = append(status.StaleUatRecords, entry.Name())
				}
			}
		}
	}

	if c.JSON {
		b, _ := json.MarshalIndent(status, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}

	fmt.Fprintf(d.Out, "sbx:     %v\n", status.SbxPresent)
	fmt.Fprintf(d.Out, "docker:  %v\n", status.DockerPresent)
	fmt.Fprintf(d.Out, "chrome:  %v\n", status.ChromePresent)
	fmt.Fprintf(d.Out, "profile: %v\n", status.ProfilePresent)
	fmt.Fprintf(d.Out, "stale:   %v\n", status.StaleUatRecords)
	return nil
}

type uatBrowserCmd struct {
	Bootstrap uatBrowserBootstrapCmd `cmd:"" help:"Bootstrap host Chrome using the fixed persistent UAT profile."`
}

type uatBrowserBootstrapCmd struct{}

func (c *uatBrowserBootstrapCmd) Run(d *cli.Deps) error {
	factory := workflowUat.NewRealBrowserFactory()
	initialURL, _ := url.Parse("about:blank")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	browser, err := factory.NewOAuthContext(ctx, initialURL, nil)
	if err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	defer browser.Close()

	fmt.Fprintln(d.Out, "UAT Chrome profile bootstrapped. Waiting (press Ctrl-C to stop)...")
	<-ctx.Done()
	return nil
}
