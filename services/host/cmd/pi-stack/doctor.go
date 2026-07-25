package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pi-stack/host/config"
)

// doctor ports the Makefile `doctor:` target into Go. Unlike the shell version
// it LEADS WITH A ONE-LINE VERDICT, then details the checks grouped in
// dependency order (keys -> ollama/models -> memory -> gog -> mcp), keeping the
// copy-pasteable `TODO: <exact command>` lines for anything not set up.
//
// It must RUN cleanly inside the sandbox, where sbx and ollama are absent: every
// probe degrades to a sane TODO rather than crashing. All the OS-touching work
// goes through a shellEnv of function values so the tests drive it hermetically.

// shellEnv abstracts the ways doctor/setup touch the host: locating a binary,
// running a command for its output, reading an env var, and dialing a local TCP
// port. Tests substitute fakes; defaultShellEnv() wires the real thing.
type shellEnv struct {
	lookPath func(name string) (string, error)
	run      func(name string, args ...string) (string, error)
	getenv   func(name string) string
	dial     func(port int) bool
	statFile func(path string) bool            // does a regular file exist at path?
	readFile func(path string) (string, error) // read a file's contents
	homeDir  func() string                     // the user's home directory ($HOME)
	// fileMode returns a path's mode bits + whether it exists (file OR dir). The
	// Secrets group's perms check uses it to flag a group/other-accessible
	// op-refs.env or its dir. Nil in tests that don't exercise perms.
	fileMode func(path string) (os.FileMode, bool)
	// writeFile writes data to path (creating parent dirs). Nil in tests so
	// seeding stays hermetic; defaultShellEnv wires the real os-backed writer.
	writeFile func(path string, data []byte, perm os.FileMode) error
	// flock serializes a cross-process critical section on lockPath (an
	// advisory exclusive file lock). Nil in tests, which run fn directly so
	// hermetic unit tests never create a real lock file (the lock path derives
	// from defaultOpRefsPath, which those tests fake anyway); defaultShellEnv
	// wires the real blocking withFlock. See withProviderRefsLock.
	flock func(lockPath string, fn func() error) error
	// probe runs an UNTRUSTED registered command with a hard timeout + capped
	// output, so doctor never hangs (or floods) on a misbehaving MCP server. It
	// returns (output, timedOut, err). Nil in tests, which fall back to run so
	// they stay hermetic; defaultShellEnv wires runWithTimeout.
	probe func(name string, args ...string) (out string, timedOut bool, err error)
	// runInteractive execs a command that may need a REAL terminal/browser (gog's
	// OAuth steps): it inherits this process's stdin/stdout/stderr instead of
	// capturing output, and is deliberately UNBOUNDED (a user signing in via a
	// browser takes as long as it takes). Nil in tests that don't exercise it.
	runInteractive func(name string, args ...string) error
}

// probeTimeout bounds every registered-command probe so doctor can never wedge
// on a hung MCP server; probeMaxOutput caps how much of its output we capture.
const (
	probeTimeout   = 5 * time.Second
	probeMaxOutput = 64 << 10 // 64KB
)

// runWithTimeout execs name+args under a hard context deadline with capped
// captured output. It is the bounded alternative to shellEnv.run for probing
// untrusted registered commands: a server that hangs is killed at probeTimeout
// rather than freezing doctor, and runaway output is truncated at
// probeMaxOutput. Returns (output, timedOut, err).
func runWithTimeout(name string, args ...string) (string, bool, error) {
	return runWithTimeoutD(probeTimeout, name, args...)
}

// runWithTimeoutD is runWithTimeout with a caller-chosen deadline, so a fast
// command (e.g. `status`'s gog auth probe) can bound itself tighter than the
// default probeTimeout.
func runWithTimeoutD(timeout time.Duration, name string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Hard wall-clock bound: if the child (or a descendant it spawned that still
	// holds stdout/stderr) is alive when the context fires, WaitDelay forces the
	// pipes closed + the process killed so CombinedOutput can't hang past it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if len(out) > probeMaxOutput {
		out = out[:probeMaxOutput]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), true, ctx.Err()
	}
	return string(out), false, err
}

// probeRun invokes the bounded env.probe when wired (the real path), else falls
// back to env.run (tests). Returns (output, timedOut, err).
func probeRun(env shellEnv, name string, args ...string) (string, bool, error) {
	if env.probe != nil {
		return env.probe(name, args...)
	}
	if env.run == nil {
		return "", false, fmt.Errorf("no runner")
	}
	out, err := env.run(name, args...)
	return out, false, err
}

// defaultShellEnv returns a shellEnv backed by the real OS.
func defaultShellEnv() shellEnv {
	return shellEnv{
		lookPath: exec.LookPath,
		run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
		getenv: os.Getenv,
		dial: func(port int) bool {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		},
		statFile: func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && !info.IsDir()
		},
		readFile: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		homeDir: func() string {
			h, _ := os.UserHomeDir()
			return h
		},
		fileMode: func(path string) (os.FileMode, bool) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, false
			}
			return fi.Mode(), true
		},
		// writeFile is LEAF-symlink-safe (parent-directory symlinks are a
		// separate, honestly out-of-scope concern — see atomicWriteInDir's doc
		// comment): the destination is never opened directly, so a leaf that is
		// itself a symlink is REPLACED by an atomic same-directory temp file +
		// rename, never followed/truncated through. Parent creation stays 0700
		// (unchanged perm posture). Shared with writeWorkspaceStateFile's exact
		// mechanism (workspacestate.go) so there is one hardened writer, not two.
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			return atomicWriteInDir(dir, filepath.Base(path), data, perm)
		},
		flock: withFlock,
		probe: runWithTimeout,
		runInteractive: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		},
	}
}

// unwrapOpRun returns the effective command doctor would trust to exec. With no
// `--` it is argv itself (a bare command). With a `--`, it unwraps the prefix
// ONLY when argv[0] is an absolute `op` binary (the real command runs via op,
// which is trusted); a `--` behind any other argv[0] returns ok=false so a
// hostile prefix is never exec'd.
func unwrapOpRun(argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return argv, true
	}
	// Only a `op run … -- <cmd>` wrapper is trusted to be unwrapped: the probe
	// execs argv[0] verbatim, so a `--` behind a FOREIGN argv[0] (e.g.
	// `/tmp/evil -- pi-stack-host mcp slack`) would run /tmp/evil. Requiring
	// basename "op" blocks that. (Residual, accepted: a registration whose argv[0]
	// is a binary literally named `op` on the exec path would pass — but that
	// presupposes an attacker who can already write arbitrary sbx registrations,
	// i.e. owns the gateway, which is outside doctor's threat model.)
	if filepath.Base(argv[0]) != "op" {
		return nil, false
	}
	return argv[sep+1:], true
}

// enabled reports whether a service name is in the configured SERVICES set.
func enabled(cfg *config.Config, name string) bool {
	for _, s := range cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}

// mcpConfigured reports whether name is in the configured MCP set (so `run`
// auto-attaches it via --mcp).
func mcpConfigured(cfg *config.Config, name string) bool {
	for _, m := range cfg.MCP {
		if m == name {
			return true
		}
	}
	return false
}

// findUpward walks up from the current working directory looking for a directory
// that contains BOTH a Makefile and the given repo-relative file, returning the
// absolute path to that file (or "" if none is found before the filesystem root).
// This is how doctor locates a repo checkout's config files (op-refs.env)
// regardless of where it was invoked from within the tree.
func findUpward(env shellEnv, rel string) string {
	if env.statFile == nil {
		return ""
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if env.statFile(filepath.Join(dir, "Makefile")) && env.statFile(filepath.Join(dir, rel)) {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveOpRefs resolves config/op-refs.env to an ABSOLUTE, canonical location
// so doctor's headless probe matches the gateway registration exactly (`make
// mcp-register` registers the gog spawn with an absolute --env-file; a relative
// one here would resolve against doctor's cwd and could probe a different file
// than the gateway actually uses). It searches, in order, and returns the FIRST
// that exists:
//  1. $PI_STACK_CONFIG's directory + op-refs.env,
//  2. a repo checkout's config/op-refs.env (walk up for Makefile + that file),
//  3. ~/.config/pi-stack/op-refs.env.
//
// Returns "" when none exists, so the caller reports "cannot verify" rather than
// probing (and blessing) a file the gateway never uses.
func resolveOpRefs(env shellEnv) string {
	// abs makes every resolved path ABSOLUTE regardless of doctor's cwd: a
	// relative $PI_STACK_CONFIG (e.g. `config/config.toml`) would otherwise yield
	// a cwd-relative op-refs path that need not match the gateway's --env-file.
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	if env.getenv != nil {
		if p := env.getenv("PI_STACK_CONFIG"); p != "" {
			cand := filepath.Join(filepath.Dir(p), "op-refs.env")
			if env.statFile != nil && env.statFile(cand) {
				return abs(cand)
			}
		}
	}
	if p := findUpward(env, filepath.Join("config", "op-refs.env")); p != "" {
		return abs(p)
	}
	if env.homeDir != nil && env.statFile != nil {
		if home := env.homeDir(); home != "" {
			cand := filepath.Join(home, ".config", "pi-stack", "op-refs.env")
			if env.statFile(cand) {
				return abs(cand)
			}
		}
	}
	return ""
}

// grepWord reports whether out contains name as a whole word (matches the
// Makefile's `grep -qw`).
func grepWord(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == '/' || r == '"' || r == '='
		}) {
			if f == name {
				return true
			}
		}
	}
	return false
}

// runDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked shellEnv and assert on the rendered output. Each group is
// built by its own builder (doctor_providers.go, doctor_ollama.go,
// doctor_memory.go, doctor_gog.go, doctor_secrets.go, doctor_mcp.go) so later
// stories can rework one group without touching the others.
func runDoctor(cfg *config.Config, env shellEnv) *report {
	r := &report{}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	r.sbxAbsent = !sbxOK

	// MCP registrations (`sbx mcp ls`), listed once and reused by the gog group
	// (its gateway registration) and the MCP group below. sbxOK (sbx PRESENT +
	// `sbx secret ls` ok) is tracked SEPARATELY from mcpOK (`sbx mcp ls` ok): on
	// the host the CLI is present but the MCP listing can fail (gateway off), and
	// that must not be reported as "sbx unavailable".
	mcpOut, mcpOK := "", false
	if sbxOK {
		if out, err := env.run("sbx", "mcp", "ls"); err == nil {
			mcpOut, mcpOK = out, true
		}
	}

	// (a) provider secrets — proxy-injected, never in the VM.
	r.groups = append(r.groups, providersGroup(sbxOut, sbxOK))
	// (b) ollama + the configured watcher/embed models.
	r.groups = append(r.groups, ollamaGroup(cfg, env))
	// (c) memory service on :11435.
	r.groups = append(r.groups, memoryGroup(cfg, env))
	// (d) gog: Google Workspace via a host-side stdio MCP server the sbx gateway
	// spawns (the slack pattern).
	r.groups = append(r.groups, gogGroup(cfg, env, mcpOut, mcpOK, sbxOK))
	// (d2) Secrets (1Password) — its OWN top-level group, honest and separate.
	r.groups = append(r.groups, secretsGroup(cfg, env))
	// (e) MCP servers registered with sbx.
	r.groups = append(r.groups, mcpGroup(cfg, env, mcpOut, mcpOK, sbxOK))

	return r
}

// runDoctorCmd is the CLI entry point wired into main's dispatch. Exit codes
// are part of the contract: 0 = nothing verified-broken that pi-stack REQUIRES
// (optional gaps and unverifiable checks never block), 1 = a POSITIVELY
// VERIFIED core failure (verdict todo/denied on a core requirement) or a
// config-load error, 2 = usage error.
func runDoctorCmd(argv []string) {
	jsonOut, verbose, err := parseDoctorArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(doctorUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack doctor: %v\n\n%s", err, doctorUsage)
		os.Exit(2)
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack doctor: %v\n", err)
		os.Exit(1)
	}
	r := runDoctor(cfg, defaultShellEnv())
	r.services = cfg.Services
	r.mcp = cfg.MCP
	if jsonOut {
		_ = writeJSONOut(os.Stdout, r.jsonView(""))
	} else {
		r.render(os.Stdout, verbose)
	}
	// Only a POSITIVELY VERIFIED core failure exits 1. Optional failures —
	// configured or not — and anything merely unverifiable exit 0. Usage
	// errors above already exit 2.
	if r.blocking() {
		os.Exit(1)
	}
}

// parseDoctorArgs validates doctor flags: -h/--help returns errHelpRequested,
// --json sets jsonOut, --verbose sets verbose (full per-check detail; the
// default is concise and collapses ready checks), any other token is a usage
// error (exit 2).
func parseDoctorArgs(argv []string) (jsonOut, verbose bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, false, errHelpRequested
		case "--json":
			jsonOut = true
		case "--verbose":
			verbose = true
		default:
			return false, false, fmt.Errorf("unknown flag %q", a)
		}
	}
	return jsonOut, verbose, nil
}
