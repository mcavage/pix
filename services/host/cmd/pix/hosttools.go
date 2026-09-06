package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"pix/host/envinfo"
	"pix/host/hosttools"
	"pix/host/lease"
	"pix/host/pixhome"
	"pix/host/session"
	"pix/host/stack"
	"pix/host/workflow/launch"
)

func runHostToolsMCP(args []string, d *cliDeps) int {
	fs := flag.NewFlagSet(envinfo.HostToolsSubcommand, flag.ContinueOnError)
	fs.SetOutput(d.Err)
	homeArg := fs.String("home", "", "Pix home")
	workspace := fs.String("workspace", "", "host workspace")
	name := fs.String("sandbox", "", "sandbox name")
	dev := fs.Bool("dev", false, "host command authority")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || !filepath.IsAbs(*homeArg) || !filepath.IsAbs(*workspace) {
		fmt.Fprintln(d.Err, "pix: invalid host tool context")
		return 2
	}
	home := pixhome.New(*homeArg)
	id, err := stack.ID(home.Home)
	if err != nil {
		return 2
	}
	prefix, _ := stack.SandboxPrefix(id)
	if len(*name) <= len(prefix) || (*name)[:len(prefix)] != prefix {
		fmt.Fprintln(d.Err, "pix: host tools require this home's sandbox")
		return 2
	}
	dir, err := lease.SandboxDir(home.StateSandboxes, *name)
	if err != nil {
		return 2
	}
	real, err := filepath.EvalSymlinks(*workspace)
	if err != nil || real != *workspace {
		fmt.Fprintln(d.Err, "pix: host workspace must be canonical")
		return 2
	}
	if err := os.Setenv(pixhome.EnvVar, home.Home); err != nil {
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	identity := envinfo.HostToolsID(home.Home, *workspace, *name, *dev)
	var mu sync.Mutex
	bound := ""
	guard := func() error {
		mu.Lock()
		defer mu.Unlock()
		refusal := errors.New("host tools require their original live Pix session")
		if !launch.SessionHostToolsMatch(*name, identity) {
			return refusal
		}
		recorded, err := lease.ReadRecord(dir)
		if err != nil {
			return refusal
		}
		if bound != "" && bound != recorded.InstanceID {
			return refusal
		}
		entry, ok := launch.FindPositivelyIdentifiedRunning(defaultShellEnv(), *name)
		if !ok || entry.InstanceID == nil || *entry.InstanceID != recorded.InstanceID {
			return refusal
		}
		census := session.CountHolders(dir, recorded.InstanceID)
		if !census.Known || census.N == 0 {
			return refusal
		}
		bound = recorded.InstanceID
		return nil
	}
	svc := &hosttools.Service{Config: hosttools.Config{Home: home, Workspace: *workspace, Pix: exe, Dev: *dev, Guard: guard, Redact: func(v string) string { return redactCreateOutput(v, createSecretValues()) }}}
	defer svc.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-signals:
			svc.Close()
			if in, ok := d.In.(*os.File); ok {
				_ = in.Close()
			}
		case <-stopped:
		}
	}()
	srv := session.NewServer(session.ServerContext{}, nil, d.In, d.Out)
	srv.Tools = svc
	if err := srv.Serve(); err != nil {
		fmt.Fprintln(d.Err, "pix: host tool transport:", err)
		return 1
	}
	return 0
}
