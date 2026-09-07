package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hosttools"
	"pix/host/lease"
	"pix/host/pixhome"
	"pix/host/sandbox"
	"pix/host/session"
	"pix/host/stack"
	nativeenv "pix/host/workflow/env"
	"pix/host/workflow/launch"
)

// The actual compiled Pix CLI owns adoption and validation; the MCP caller
// must reach it without granting trust or replacing the user's default.
func TestHostToolsMCPAdoptsEnvironmentThroughRealPix(t *testing.T) {
	bin := buildPixBinary(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := pixhome.New(filepath.Join(root, "home"))
	workspace := filepath.Join(root, "workspace")
	source := filepath.Join(workspace, "candidate")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := &hosttools.Service{Config: hosttools.Config{Home: home, Workspace: workspace, Pix: bin, Guard: func() error { return nil }}}
	defer svc.Close()
	var out bytes.Buffer
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pix_env_add","arguments":{"source":"candidate","name":"candidate"}}}` + "\n"
	srv := session.NewServer(session.ServerContext{}, nil, strings.NewReader(input), &out)
	srv.Tools = svc
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("%s", out.String())
	}
	var job map[string]any
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &job); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"job_id": job["job_id"]})
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := svc.Call("pix_job_status", raw)
		if err != nil {
			t.Fatal(err)
		}
		var status map[string]any
		_ = json.Unmarshal([]byte(out), &status)
		if status["status"] == "finished" {
			if status["exit_code"] != float64(0) {
				t.Fatalf("adopt failed: %s", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adoption hung")
		}
		time.Sleep(10 * time.Millisecond)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(home.Envs, "candidate"))
	if err != nil || target != source {
		t.Fatalf("target %q, %v", target, err)
	}
	if _, err := os.Stat(home.ConfigTOML); !os.IsNotExist(err) {
		t.Fatal("adoption changed config")
	}
	if _, err := os.Stat(home.StateTrust); !os.IsNotExist(err) {
		t.Fatal("adoption granted trust")
	}
}

func TestHostToolResetNamesExcludeForeignRegistrations(t *testing.T) {
	base := "pix-session-0123456789abcdef"
	own := base + "-0123456789abcdef"
	got := scopedHostToolRegistrations(base, []string{own, "pix-session-aaaaaaaaaaaaaaaa-0123456789abcdef", base + "-bad", "pix-memory-0123456789abcdef", base})
	if len(got) != 1 || got[0] != own {
		t.Fatalf("unsafe reset selection: %v", got)
	}
}

// Exercise the shipped hidden dispatch with real reference locks and sbx
// subprocess probes. A stale development registration cannot bind a normal
// sandbox, and a connected server cannot follow a replacement instance.
func TestHostToolsHiddenDispatchLifetime(t *testing.T) {
	bin := buildPixBinary(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := pixhome.New(filepath.Join(root, "home"))
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", home.Home)
	id, err := stack.ID(home.Home)
	if err != nil {
		t.Fatal(err)
	}
	name, err := sandbox.NameForStack(id, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := lease.SandboxDir(home.StateSandboxes, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home.StateSandboxes, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.CreateRecord(dir, "instance-1"); err != nil {
		t.Fatal(err)
	}
	// Exercise the real default CLI workspace ("."), not a hand-built
	// fingerprint that already agrees with the MCP registration.
	t.Chdir(workspace)
	opts, err := parseRunOpts(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.Name = name
	fp, _ := json.Marshal(launch.SessionFingerprint(&config.Config{}, opts))
	if err := os.WriteFile(filepath.Join(dir, "fingerprint.json"), fp, 0600); err != nil {
		t.Fatal(err)
	}
	listing := func(instance string) {
		t.Helper()
		body := fmt.Sprintf(`[{"name":%q,"state":"running","instance_id":%q}]`, name, instance)
		if err := os.WriteFile(filepath.Join(home.Home, "listing.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	listing("instance-1")
	fake := filepath.Join(root, "sbx")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat \"$PIX_HOME/listing.json\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	holder, err := session.HoldInteractiveRoot(dir, "tree-1", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	start := func(dev bool) (*exec.Cmd, *bufio.Scanner, func(string) bool) {
		t.Helper()
		args := []string{envinfo.HostToolsSubcommand, "--home", home.Home, "--workspace", workspace, "--sandbox", name}
		if dev {
			args = append(args, "--dev")
		}
		cmd := exec.Command(bin, args...)
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
		scanner := bufio.NewScanner(out)
		call := func(tool string) bool {
			t.Helper()
			_, err := fmt.Fprintf(in, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`+"\n", tool)
			if err != nil {
				t.Fatal(err)
			}
			if !scanner.Scan() {
				t.Fatal("host tool transport closed")
			}
			var response struct {
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			return !response.Result.IsError
		}
		return cmd, scanner, call
	}
	_, _, normal := start(false)
	if !normal("pix_host_info") {
		t.Fatal("live normal session refused")
	}
	_, _, dev := start(true)
	if dev("pix_host_info") {
		t.Fatal("stale development registration gained normal session authority")
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	if normal("pix_host_info") {
		t.Fatal("session without holder allowed")
	}
	if err := os.Remove(filepath.Join(dir, "record.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.CreateRecord(dir, "instance-2"); err != nil {
		t.Fatal(err)
	}
	holder, err = session.HoldInteractiveRoot(dir, "tree-2", "instance-2")
	if err != nil {
		t.Fatal(err)
	}
	listing("instance-2")
	if normal("pix_host_info") {
		t.Fatal("connected registration followed replacement instance")
	}
}

func TestRunEffectiveHostToolRegistration(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := pixhome.New(filepath.Join(root, "home"))
	t.Setenv("PIX_HOME", home.Home)
	t.Chdir(root)
	name, err := sandbox.Name(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	opts := launch.RunOpts{Workspace: root, Name: name}
	normal, err := runEffectiveInput(cfg, opts, launch.EnvSelection{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	find := func(servers []envinfo.MCPWrapperFact) envinfo.MCPWrapperFact {
		for _, server := range servers {
			if envinfo.IsSessionMCPName(server.Name) {
				return server
			}
		}
		t.Fatal("host tools absent from real launch")
		return envinfo.MCPWrapperFact{}
	}
	server := find(normal.MCPServers)
	if len(server.Args) < 7 || server.Args[0] != envinfo.HostToolsSubcommand {
		t.Fatalf("unwired host tools: %+v", server)
	}
	preview, err := nativeenv.ComputeEffective(home, "", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Template != "" {
		t.Fatalf("preview overrides the kit image: %q", preview.Template)
	}
	previewServer := find(preview.MCPServers)
	if fmt.Sprint(previewServer) != fmt.Sprint(server) {
		t.Fatalf("preview mismatch: %+v vs %+v", previewServer, server)
	}
	fp := launch.SessionFingerprint(cfg, opts)
	if fp["host_tools"] != envinfo.HostToolsID(home.Home, root, name, false) {
		t.Fatal("guard identity differs from launch")
	}
	opts.Dev = true
	dev, err := runEffectiveInput(cfg, opts, launch.EnvSelection{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	devServer := find(dev.MCPServers)
	if devServer.Name == server.Name || devServer.Args[len(devServer.Args)-1] != "--dev" {
		t.Fatal("development authority not fixed at launch")
	}
	if launch.SessionFingerprint(cfg, opts)["host_tools"] == fp["host_tools"] {
		t.Fatal("attach can silently change authority")
	}
	t.Setenv("PIX_HOST_TOOLS_DISABLED", "1")
	trial, err := runEffectiveInput(cfg, opts, launch.EnvSelection{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range trial.MCPServers {
		if envinfo.IsSessionMCPName(server.Name) {
			t.Fatal("trial received host tools")
		}
	}
}

func TestHostToolsWorkspaceIdentityMatchesEffectiveDocument(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", filepath.Join(root, "home"))
	t.Chdir(workspace)
	for _, path := range []string{".", "../workspace", workspace, link} {
		for _, dev := range []bool{false, true} {
			opts, err := parseRunOpts([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			opts.Name, opts.Dev = "pix-test", dev
			in, err := runEffectiveInput(&config.Config{}, opts, launch.EnvSelection{}, "0.1.82")
			if err != nil {
				t.Fatal(err)
			}
			fp := launch.SessionFingerprint(&config.Config{}, opts)
			want := envinfo.HostToolsID(filepath.Join(root, "home"), workspace, opts.Name, dev)
			if fp["host_tools"] != want || in.PrimaryWorkspace.Path != workspace {
				t.Fatalf("path %q dev %v: fingerprint %q workspace %q", path, dev, fp["host_tools"], in.PrimaryWorkspace.Path)
			}
			found := false
			for _, server := range in.MCPServers {
				if envinfo.IsSessionMCPName(server.Name) {
					found = strings.HasSuffix(server.Name, "-"+want)
				}
			}
			if !found {
				t.Fatalf("path %q dev %v: MCP registration mismatches session", path, dev)
			}
		}
	}
}
