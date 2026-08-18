// memory_matrix.go is the isolated, host-backed memory UAT coverage
// HANDOFF-MEMORY-UAT.md asked for: real candidate pix/pix-host binaries,
// a deterministic in-process fake Ollama, and a run-local config/state/db —
// never port 11435, never the real ~/.config/pix or ~/.local/{share,state}/pix.
//
// executeCandidateSmoke calls Run through Runner.memoryMatrix after the
// candidate binaries are built and before the sandbox launches. Every check below talks to the candidate
// binaries directly — no sandbox, no docker, no sbx — because the memory
// daemon's whole surface (JSON-RPC :MEMORY_PORT, `pix memory`, `pix-host
// memory`/`serve`) is host-only. Each check gets its own ephemeral port, its
// own fake Ollama, and its own scratch directory, so a failure in one never
// contaminates another, and writes exactly one bounded log artifact under
// stepsDir (candidateLogMaxBytes, the same cap the candidate pix log uses).
//
// Every check here does the REAL thing (starts a real daemon, makes a real
// JSON-RPC call, reads the real sqlite file) rather than asserting against
// this package's own understanding of the behavior — that's the entire point
// of "host-backed" coverage the handoff distinguished from repository unit
// tests. A check that cannot be performed this way (no safe seam exists) must
// return an error, never silently pass: see Run's guard on missing candidate
// binaries.
package uatmatrix

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// memWatcherDailyBudgetUAT mirrors memory.go's memWatcherDailyBudget. It is
// duplicated, not imported (this package cannot import the host's `main`
// package), so a future change to the real budget makes this check fail
// loudly instead of silently drifting.
const (
	memWatcherDailyBudgetUAT = 10
	matrixLogMaxBytes        = 1024 * 1024
)

type cappedLogWriter struct {
	mu        sync.Mutex
	file      *os.File
	remaining int
	truncated bool
}

func (w *cappedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLen := len(p)
	if w.remaining <= 0 {
		if !w.truncated {
			_, _ = w.file.WriteString("\n[output truncated at 1 MiB]\n")
			w.truncated = true
		}
		return originalLen, nil
	}
	toWrite := p
	if len(toWrite) > w.remaining {
		toWrite = toWrite[:w.remaining]
	}
	written, err := w.file.Write(toWrite)
	w.remaining -= written
	if err != nil {
		return written, err
	}
	if written < len(toWrite) {
		return written, io.ErrShortWrite
	}
	return originalLen, nil
}

// Inputs identifies the two run-local paths the matrix is allowed to use.
type Inputs struct {
	OutDir   string
	StepsDir string
}

// Run executes the memory checks against candidate binaries, in isolation,
// before the sandbox launches.
func Run(ctx context.Context, in Inputs) error {
	pixBin := filepath.Join(in.OutDir, "pix")
	pixHostBin := filepath.Join(in.OutDir, "pix-host")
	for _, p := range []string{pixBin, pixHostBin} {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("memory matrix: candidate binary missing (%s): %w", p, err)
		}
		if fi.IsDir() {
			return fmt.Errorf("memory matrix: candidate binary path is a directory: %s", p)
		}
	}

	matrixRoot := filepath.Join(filepath.Dir(in.StepsDir), "memory-matrix")
	if err := os.MkdirAll(matrixRoot, 0700); err != nil {
		return fmt.Errorf("memory matrix: create scratch root: %w", err)
	}

	checks := []struct {
		name string
		fn   func(context.Context, io.Writer, string, string, string) error
	}{
		{"cold_start_no_ollama", checkColdStartNoOllama},
		{"repeated_recall_star", checkRepeatedRecallStar},
		{"stale_daemon_no_success", checkStaleDaemonNoSuccess},
		{"explicit_mode_no_watcher", checkExplicitModeNoWatcher},
		{"experimental_auto_budget", checkExperimentalAutoBudget},
		{"remember_source_watcher_spoof", checkRememberSourceSpoof},
		{"v1_migration", checkV1Migration},
		{"forget_missing_exit1", checkForgetMissingExit1},
		{"plugin_restart_retains_row", checkPluginRestartRetainsRow},
	}

	for _, c := range checks {
		phaseDir := filepath.Join(matrixRoot, c.name)
		if err := os.MkdirAll(phaseDir, 0700); err != nil {
			return fmt.Errorf("memory matrix: create phase dir %s: %w", c.name, err)
		}
		fn := c.fn
		if err := runMatrixCheck(in.StepsDir, c.name, func(lw io.Writer) error {
			return fn(ctx, lw, pixBin, pixHostBin, phaseDir)
		}); err != nil {
			return err
		}
	}
	return nil
}

// runMatrixCheck writes ONE bounded artifact per check (steps/memory_<name>.log,
// capped at candidateLogMaxBytes like the candidate pix log), so a failure
// deep in the matrix is diagnosable from the run's steps directory alone.
func runMatrixCheck(stepsDir, name string, fn func(io.Writer) error) error {
	logPath := filepath.Join(stepsDir, "memory_"+name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create memory check log %s: %w", name, err)
	}
	lw := &cappedLogWriter{file: f, remaining: matrixLogMaxBytes}
	fmt.Fprintf(lw, "=== memory check: %s ===\n", name)
	checkErr := fn(lw)
	if checkErr != nil {
		fmt.Fprintf(lw, "RESULT: FAIL: %v\n", checkErr)
	} else {
		fmt.Fprintf(lw, "RESULT: PASS\n")
	}
	_ = f.Close()
	if checkErr != nil {
		return fmt.Errorf("%s: %w (log: steps/memory_%s.log)", name, checkErr, name)
	}
	return nil
}

// --- isolation helpers -------------------------------------------------

// isolatedEnv builds a run-local env for one phase: PATH/HOME/TMPDIR/LANG
// pass through (needed to exec anything at all), every XDG root and
// PIX_CONFIG point inside phaseDir, and a default config.toml declares
// services=["memory"] so `pix memory ...`'s EnsureUp autostart actually has
// something to start (config.DefaultServices is empty on purpose upstream).
// This is what keeps every check off the real ~/.config/pix and
// ~/.local/{share,state}/pix.
func isolatedEnv(phaseDir string, extra ...string) ([]string, error) {
	var base []string
	for _, e := range os.Environ() {
		for _, allow := range []string{"PATH=", "TMPDIR=", "TMP=", "TEMP=", "LANG=", "LC_ALL="} {
			if strings.HasPrefix(e, allow) {
				base = append(base, e)
				break
			}
		}
	}
	homeDir := filepath.Join(phaseDir, "home")
	cfgDir := filepath.Join(phaseDir, "config")
	dataDir := filepath.Join(phaseDir, "data")
	stateDir := filepath.Join(phaseDir, "state")
	cacheDir := filepath.Join(phaseDir, "cache")
	for _, d := range []string{homeDir, cfgDir, dataDir, stateDir, cacheDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	cfgFile := filepath.Join(cfgDir, "config.toml")
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		if err := os.WriteFile(cfgFile, []byte("services = [\"memory\"]\n"), 0600); err != nil {
			return nil, err
		}
	}
	base = append(base,
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+cfgDir,
		"XDG_DATA_HOME="+dataDir,
		"XDG_STATE_HOME="+stateDir,
		"XDG_CACHE_HOME="+cacheDir,
		"PIX_CONFIG="+cfgFile,
	)
	return append(base, extra...), nil
}

// setEnv overrides key in env, filtering any existing entry first: os/exec
// does not document which of two duplicate-keyed entries a child observes, so
// appending a second one is not a safe override.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+val)
}

// freePort picks an ephemeral, currently-unused loopback port and refuses
// 11435: the whole point of this matrix is that it never touches the real
// memory daemon's port (vanishingly unlikely to collide, but the guard makes
// that a proven property, not a probabilistic one).
func freePort() (int, error) {
	for i := 0; i < 5; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		_, portStr, _ := net.SplitHostPort(ln.Addr().String())
		_ = ln.Close()
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return 0, err
		}
		if port != 11435 {
			return port, nil
		}
	}
	return 0, errors.New("freePort: kept receiving 11435")
}

// memoryEnv builds the isolated env for one memory-daemon phase: its own
// port, bind, db path, and Ollama endpoint. captureMode is set as
// MEMORY_CAPTURE_MODE only when non-empty (empty leaves the default,
// explicit, in effect).
func memoryEnv(phaseDir string, port int, ollamaURL, captureMode string) ([]string, error) {
	dbPath := filepath.Join(phaseDir, "memory.db")
	extra := []string{
		"MEMORY_PORT=" + strconv.Itoa(port),
		"MEMORY_BIND=127.0.0.1",
		"MEMORY_DB=" + dbPath,
		"OLLAMA_HOST=" + ollamaURL,
	}
	if captureMode != "" {
		extra = append(extra, "MEMORY_CAPTURE_MODE="+captureMode)
	}
	return isolatedEnv(phaseDir, extra...)
}

// --- deterministic in-process fake Ollama -------------------------------

// fakeOllama serves /api/embed, /api/chat, and /api/show deterministically
// and in-process, so no check here ever depends on a real Ollama being
// installed. Every request is counted, so a check can prove a code path made
// ZERO Ollama traffic — the explicit-capture and cold-start requirements are
// otherwise unverifiable from the outside.
type fakeOllama struct {
	srv *httptest.Server

	mu      sync.Mutex
	embed   int
	chat    int
	show    int
	chatLog []string

	// chatResponder builds the watcher's facts/corrections for one incoming
	// user message. nil (the default) always answers empty, i.e. "the
	// watcher extracted nothing."
	chatResponder func(user string) (facts, corrections []string)

	embedCounter atomic.Int64
}

// embedVectorDim is generous headroom above the largest number of embed
// calls any single check makes, so the one-hot vectors below stay pairwise
// orthogonal (cosine 0) for the whole matrix — never close enough to
// findSimilar's 0.9 dedupe threshold to risk collapsing two genuinely
// distinct facts into one row.
const embedVectorDim = 256

func newFakeOllama() *fakeOllama {
	f := &fakeOllama{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embed", f.handleEmbed)
	mux.HandleFunc("/api/chat", f.handleChat)
	mux.HandleFunc("/api/show", f.handleShow)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeOllama) Close()      { f.srv.Close() }
func (f *fakeOllama) URL() string { return f.srv.URL }

func (f *fakeOllama) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.embed + f.chat + f.show
}

func (f *fakeOllama) chatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chat
}

// handleEmbed answers with a one-hot vector keyed by a monotonically
// increasing counter, NOT by hashing the input: two random hash-derived
// vectors of modest dimension can land closer than the 0.9 dedupe threshold
// purely by chance, which would make the budget check in
// checkExperimentalAutoBudget flaky. One-hot vectors are exactly orthogonal,
// deterministically, every time.
func (f *fakeOllama) handleEmbed(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.embed++
	f.mu.Unlock()
	idx := int(f.embedCounter.Add(1)-1) % embedVectorDim
	vec := make([]float64, embedVectorDim)
	vec[idx] = 1
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
}

func (f *fakeOllama) handleChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	var userMsg string
	for _, m := range req.Messages {
		if m.Role == "user" {
			userMsg = m.Content
		}
	}
	f.mu.Lock()
	f.chat++
	f.chatLog = append(f.chatLog, userMsg)
	responder := f.chatResponder
	f.mu.Unlock()

	facts, corrections := []string{}, []string{}
	if responder != nil {
		facts, corrections = responder(userMsg)
		if facts == nil {
			facts = []string{}
		}
		if corrections == nil {
			corrections = []string{}
		}
	}
	content, _ := json.Marshal(map[string]any{"facts": facts, "corrections": corrections})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": string(content)}})
}

func (f *fakeOllama) handleShow(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.show++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// --- process + RPC helpers ----------------------------------------------

// procHandle wraps a started *exec.Cmd with a Wait result safe for multiple
// readers. The goroutine calls exec.Cmd.Wait exactly once, stores its result,
// then closes done; stop and waitExit may both observe that closed channel.
type procHandle struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func startProcess(ctx context.Context, bin string, args, env []string, dir, logPath string) (*procHandle, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	// Give every candidate daemon and any plugin children it spawns their own
	// process group. Cleanup can then terminate the whole run-local tree rather
	// than orphaning a plugin if the serve parent ignores TERM or the UAT context
	// is cancelled.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, err
	}
	p := &procHandle{cmd: cmd, done: make(chan struct{})}
	go func() {
		waitErr := cmd.Wait()
		p.mu.Lock()
		p.err = waitErr
		p.mu.Unlock()
		_ = lf.Close()
		close(p.done)
	}()
	return p, nil
}

// stop sends SIGTERM and waits up to timeout, escalating to SIGKILL. It
// always blocks until the process is actually reaped, so a caller that opens
// the same sqlite file right after stop() returns never races the daemon's
// own close.
func (p *procHandle) stop(timeout time.Duration) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	parentDone := false
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !parentDone {
			select {
			case <-p.done:
				parentDone = true
			default:
			}
		}
		if !processGroupAlive(pgid) {
			if !parentDone {
				<-p.done
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if !parentDone {
		<-p.done
	}
}

// waitExit waits up to timeout for the process to exit on its own (no
// signal sent), reporting whether it did.
func (p *procHandle) waitExit(timeout time.Duration) (err error, exited bool) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func waitTCP(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// rpcCall makes one JSON-RPC 2.0 call against the memory daemon's :port —
// the exact wire contract memoryStoreMux (serve_plugin.go) serves, whether
// behind the bare `pix-host memory` daemon or the supervised `pix-host serve`
// unit.
func rpcCall(ctx context.Context, port int, method string, params map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d", port), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	if e, ok := out["error"]; ok && e != nil {
		return nil, fmt.Errorf("rpc %s: %v", method, e)
	}
	r, _ := out["result"].(map[string]any)
	return r, nil
}

func waitRPCReady(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := rpcCall(ctx, port, "identity", map[string]any{}); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("memory rpc never became ready on :%d: %w", port, lastErr)
}

// waitForStatCount polls `stats` until the given field reaches at least want,
// bounding the wait on an async memCapture goroutine landing its write.
func waitForStatCount(ctx context.Context, port int, field string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastGot float64
	for time.Now().Before(deadline) {
		res, err := rpcCall(ctx, port, "stats", map[string]any{})
		if err == nil {
			if v, ok := res[field].(float64); ok {
				lastGot = v
				if int(v) >= want {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("stats.%s never reached %d (last seen %.0f)", field, want, lastGot)
}

func readPidFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func processGroupAlive(pgid int) bool {
	return syscall.Kill(-pgid, syscall.Signal(0)) == nil
}

func candidateProcessGroup(pgid int, pixHostBin string) (bool, error) {
	out, err := exec.Command("ps", "-e", "-o", "pgid=", "-o", "command=").Output()
	if err != nil {
		return false, fmt.Errorf("ps process groups: %w", err)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		gotPGID, parseErr := strconv.Atoi(fields[0])
		if parseErr != nil || gotPGID != pgid {
			continue
		}
		found = true
		command := strings.Join(fields[1:], " ")
		if !strings.HasPrefix(command, pixHostBin+" ") {
			return false, fmt.Errorf("process group %d contains foreign command %q", pgid, command)
		}
	}
	return found, nil
}

func killPid(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

func processIdentity(pid int) (ppid int, command string, err error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=", "-o", "command=").Output()
	if err != nil {
		return 0, "", fmt.Errorf("ps pid %d: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("ps pid %d returned an incomplete row: %q", pid, out)
	}
	ppid, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", fmt.Errorf("parse ppid for pid %d: %w", pid, err)
	}
	command = strings.Join(fields[1:], " ")
	return ppid, command, nil
}

func stopVerifiedServe(pid int, pixHostBin string, timeout time.Duration) error {
	_, command, err := processIdentity(pid)
	marker := pixHostBin + " serve"
	if err != nil {
		if processAlive(pid) {
			return err
		}
		if !processGroupAlive(pid) {
			return nil
		}
		candidate, groupErr := candidateProcessGroup(pid, pixHostBin)
		if groupErr != nil {
			return groupErr
		}
		if !candidate {
			return fmt.Errorf("refusing to signal unverified process group %d", pid)
		}
	} else if !strings.HasPrefix(command, marker) {
		return fmt.Errorf("refusing to signal pid %d: command %q does not start with %q", pid, command, marker)
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && processGroupAlive(pid) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The parent may already be gone while a plugin in its session remains.
	// The group id is the verified serve pid (EnsureUp starts serve with Setsid),
	// so escalation targets that same run-local tree rather than a bare pid.
	if processAlive(pid) {
		_, command, err = processIdentity(pid)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(command, marker) {
			return fmt.Errorf("refusing to escalate pid %d after identity changed to %q", pid, command)
		}
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && processGroupAlive(pid) {
		return err
	}
	return nil
}

// findPluginMemoryChild locates the supervised memory plugin's child process:
// `pix-host serve` self-execs as `<selfPath> plugin memory` (supervise/service.go),
// so matching the candidate's own pix-host path plus "plugin memory" in the
// full command line is exact for this run (the path includes the run's own
// unique OutDir) and cannot be confused with an unrelated pix-host elsewhere
// on the host.
func findPluginMemoryChild(pixHostBin string, parentPid int) (int, error) {
	out, err := exec.Command("ps", "-e", "-o", "pid=", "-o", "ppid=", "-o", "command=").Output()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	marker := pixHostBin + " plugin memory"
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		command := strings.Join(fields[2:], " ")
		if pidErr == nil && ppidErr == nil && ppid == parentPid && strings.HasPrefix(command, marker) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no child of pid %d found with command prefix %q", parentPid, marker)
}

// --- sqlite fixture helpers (v1 migration check) -------------------------

func openSQLite(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}

// writeV1Fixture builds a pre-migration (user_version=1) memory db with one
// row per historical classification migrateMemorySchema must sort correctly:
// an explicit user row, an explicit cli row, a legacy watcher row, a legacy
// row from a source no longer recognized ("unknown"), and a legacy
// durability='perishable' row. malformed=true drops the `durability` column
// migrateMemorySchema's SECOND soft-delete sweep queries, so that sweep fails
// mid-transaction and the whole migration must roll back rather than
// half-apply — this is deliberately AFTER a successful first sweep runs, to
// prove the rollback undoes already-executed work, not just an early bail.
func writeV1Fixture(path string, malformed bool) error {
	db, err := openSQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()

	cols := "rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL, " +
		"content_hash TEXT NOT NULL, "
	if !malformed {
		cols += "durability TEXT NOT NULL, "
	}
	cols += "confidence REAL NOT NULL, frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, " +
		"access_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, " +
		"source TEXT NOT NULL, tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT"
	schema := "CREATE TABLE memories (" + cols + ");\nCREATE VIRTUAL TABLE memories_fts USING fts5(content);\n"
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create v1 schema: %w", err)
	}

	type fixtureRow struct{ id, kind, content, source, durability string }
	rows := []fixtureRow{
		{"row-user-1", "fact", "user row one", "user", "durable"},
		{"row-cli-1", "fact", "cli row one", "cli", "durable"},
		{"row-watcher-1", "fact", "legacy watcher row", "watcher", "durable"},
		{"row-unknown-1", "fact", "legacy unknown-source row", "some-old-extension", "durable"},
		{"row-perishable-1", "fact", "legacy perishable row", "user", "perishable"},
	}
	for i, r := range rows {
		hash := fmt.Sprintf("hash-%d", i)
		createdAt := time.Now().UTC().Format(time.RFC3339Nano)
		var execErr error
		if malformed {
			_, execErr = db.Exec(`INSERT INTO memories (id, kind, content, content_hash, confidence, source, created_at)
				VALUES (?,?,?,?,?,?,?)`, r.id, r.kind, r.content, hash, 0.8, r.source, createdAt)
		} else {
			_, execErr = db.Exec(`INSERT INTO memories (id, kind, content, content_hash, durability, confidence, source, created_at)
				VALUES (?,?,?,?,?,?,?,?)`, r.id, r.kind, r.content, hash, r.durability, 0.8, r.source, createdAt)
		}
		if execErr != nil {
			return fmt.Errorf("insert fixture row %s: %w", r.id, execErr)
		}
		var rowid int64
		if err := db.QueryRow("SELECT rowid FROM memories WHERE id = ?", r.id).Scan(&rowid); err != nil {
			return err
		}
		if _, err := db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, r.content); err != nil {
			return err
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		return err
	}
	return nil
}

func inspectMemoriesTable(path string) (count int, contents map[string]string, err error) {
	db, err := openSQLite(path)
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	contents = map[string]string{}
	rows, err := db.Query("SELECT id, content FROM memories")
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return 0, nil, err
		}
		contents[id] = content
		count++
	}
	return count, contents, rows.Err()
}

func inspectMigratedDB(path string) (version, count int, contents map[string]string, deletedByID map[string]bool, err error) {
	db, err := openSQLite(path)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	defer db.Close()
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	contents = map[string]string{}
	deletedByID = map[string]bool{}
	rows, qerr := db.Query("SELECT id, content, deleted_at FROM memories")
	if qerr != nil {
		return version, 0, nil, nil, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		var deletedAt sql.NullString
		if serr := rows.Scan(&id, &content, &deletedAt); serr != nil {
			return version, 0, nil, nil, serr
		}
		contents[id] = content
		deletedByID[id] = deletedAt.Valid && deletedAt.String != ""
		count++
	}
	return version, count, contents, deletedByID, rows.Err()
}

func inspectRollbackState(path string) (version, count, deleted int, err error) {
	db, err := openSQLite(path)
	if err != nil {
		return
	}
	defer db.Close()
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&count); err != nil {
		return
	}
	err = db.QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL").Scan(&deleted)
	return
}

// --- the checks ----------------------------------------------------------

// checkColdStartNoOllama proves buildMemStore/newMemStore make no boot-time
// probe of Ollama: a bare `pix-host memory` daemon must open its port with
// zero requests observed by the fake Ollama it is pointed at.
func checkColdStartNoOllama(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "")
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "starting bare memory daemon on 127.0.0.1:%d (OLLAMA_HOST=%s)\n", port, ollama.URL())
	startedAt := time.Now()
	proc, err := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, "daemon.log"))
	if err != nil {
		return fmt.Errorf("start pix-host memory: %w", err)
	}
	defer proc.stop(5 * time.Second)

	if err := waitRPCReady(ctx, port, 10*time.Second); err != nil {
		return err
	}
	coldElapsed := time.Since(startedAt)
	total := ollama.total()
	fmt.Fprintf(lw, "daemon ready in %s; fake Ollama saw %d request(s) during cold start\n", coldElapsed.Round(time.Millisecond), total)
	if total != 0 {
		return fmt.Errorf("cold start made %d request(s) to Ollama, want 0", total)
	}
	return nil
}

// checkRepeatedRecallStar exercises `pix memory recall '*'` through the
// candidate CLI's real EnsureUp autostart path: the first call spawns
// `pix-host serve` cold, the second must be silent (no autostart/"ready"
// chatter, no "updated" wording) and must not have restarted it.
func checkRepeatedRecallStar(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "")
	if err != nil {
		return err
	}

	run := func(label string) (stdout, stderr string, err error) {
		cmd := exec.CommandContext(ctx, pixBin, "memory", "recall", "*")
		cmd.Env = env
		cmd.Dir = phaseDir
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		err = cmd.Run()
		fmt.Fprintf(lw, "--- %s ---\n$ pix memory recall '*'\nstdout: %s\nstderr: %s\nerr: %v\n", label, outBuf.String(), errBuf.String(), err)
		return outBuf.String(), errBuf.String(), err
	}

	coldStartedAt := time.Now()
	if _, _, err := run("cold"); err != nil {
		// EnsureUp may have detached a daemon before the primary RPC failed. Read
		// and verify the isolated pidfile on this failure path too, so a failed
		// check cannot leak a candidate serve process into the host namespace.
		pidPath := filepath.Join(phaseDir, "state", "pix", "serve.pid")
		if leakedPID, pidErr := readPidFile(pidPath); pidErr == nil {
			_ = stopVerifiedServe(leakedPID, pixHostBin, 5*time.Second)
		}
		return fmt.Errorf("first (cold) recall failed: %w", err)
	}
	coldElapsed := time.Since(coldStartedAt)

	pidPath := filepath.Join(phaseDir, "state", "pix", "serve.pid")
	pid1, err := readPidFile(pidPath)
	if err != nil {
		return fmt.Errorf("read serve pidfile after cold recall: %w", err)
	}
	defer func() {
		if stopErr := stopVerifiedServe(pid1, pixHostBin, 5*time.Second); stopErr != nil {
			fmt.Fprintf(lw, "cleanup warning: %v\n", stopErr)
		}
	}()

	warmStartedAt := time.Now()
	stdout2, stderr2, err := run("warm")
	if err != nil {
		return fmt.Errorf("second (warm) recall failed: %w", err)
	}

	warmElapsed := time.Since(warmStartedAt)
	fmt.Fprintf(lw, "timings: cold=%s warm=%s\n", coldElapsed.Round(time.Millisecond), warmElapsed.Round(time.Millisecond))
	pid2, err := readPidFile(pidPath)
	if err != nil {
		return fmt.Errorf("read serve pidfile after warm recall: %w", err)
	}
	if pid1 != pid2 {
		_ = stopVerifiedServe(pid2, pixHostBin, 5*time.Second)
		return fmt.Errorf("serve pid changed across repeated recall (%d -> %d): daemon was restarted", pid1, pid2)
	}
	if !processAlive(pid2) {
		return fmt.Errorf("serve pid %d recorded but not alive after warm recall", pid2)
	}
	for _, noisy := range []string{"starting pix services", "pix services ready", "did not update"} {
		if strings.Contains(stderr2, noisy) {
			return fmt.Errorf("second recall printed autostart/update chatter (%q) against an already-warm daemon: %q", noisy, stderr2)
		}
	}
	if strings.Contains(stdout2, "updated") || strings.Contains(stderr2, "updated") {
		return fmt.Errorf("second recall mentions 'updated' wording (stdout=%q stderr=%q)", stdout2, stderr2)
	}
	if total := ollama.total(); total != 0 {
		return fmt.Errorf("recall '*' triggered %d unexpected Ollama request(s)", total)
	}
	return nil
}

// checkExplicitModeNoWatcher proves explicit (the default) capture mode
// rejects `observe` immediately, with zero watcher/embed traffic.
// checkStaleDaemonNoSuccess points the candidate CLI at an identified but
// version-mismatched memory endpoint. Read-side commands must neither recycle
// it nor print success wording that implies convergence; explicit lifecycle
// reconciliation is deliberately outside this isolated UAT because invoking a
// managed launchd service would cross the normal host-service boundary.
func checkStaleDaemonNoSuccess(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		result := map[string]any{}
		switch method {
		case "identity":
			result = map[string]any{"name": "pix-memory", "version": "stale-uat-version", "port": port, "db_path": filepath.Join(phaseDir, "foreign.db"), "ready": true, "degraded_reason": ""}
		case "recall":
			result = map[string]any{"hits": []any{}}
		default:
			result = map[string]any{"ok": true}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	env, err := isolatedEnv(phaseDir, "MEMORY_PORT="+strconv.Itoa(port), "MEMORY_BIND=127.0.0.1", "MEMORY_DB="+filepath.Join(phaseDir, "unused.db"), "PIX_NO_AUTOSERVE=1")
	if err != nil {
		return err
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, pixBin, args...)
		cmd.Env = env
		cmd.Dir = phaseDir
		out, runErr := cmd.CombinedOutput()
		fmt.Fprintf(lw, "$ pix %s\n%s\nerr: %v\n", strings.Join(args, " "), out, runErr)
		return string(out), runErr
	}
	recallOut, err := run("memory", "recall", "*")
	if err != nil {
		return fmt.Errorf("recall against stale identified daemon: %w", err)
	}
	statusOut, _ := run("serve", "status")
	combined := strings.ToLower(recallOut + "\n" + statusOut)
	for _, success := range []string{"updated pix services", "pix services ready", "verified", "configured", "enabled"} {
		if strings.Contains(combined, success) {
			return fmt.Errorf("stale daemon output contains unverified success wording %q", success)
		}
	}
	if calls.Load() == 0 {
		return fmt.Errorf("candidate CLI never contacted the stale endpoint")
	}
	return nil
}

func checkExplicitModeNoWatcher(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "explicit")
	if err != nil {
		return err
	}
	proc, err := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, "daemon.log"))
	if err != nil {
		return err
	}
	defer proc.stop(5 * time.Second)
	if err := waitRPCReady(ctx, port, 10*time.Second); err != nil {
		return err
	}

	res, err := rpcCall(ctx, port, "observe", map[string]any{"user": "please always use tabs, never spaces, in every file"})
	if err != nil {
		return fmt.Errorf("observe rpc: %w", err)
	}
	fmt.Fprintf(lw, "observe result: %#v\n", res)
	if accepted, _ := res["accepted"].(bool); accepted {
		return fmt.Errorf("observe accepted in explicit mode, want rejected")
	}
	reason, _ := res["reason"].(string)
	if !strings.Contains(reason, "explicit") {
		return fmt.Errorf("observe rejection reason %q does not mention explicit mode", reason)
	}
	// Nothing should have been scheduled (accepted was false), but give any
	// stray goroutine a moment so the zero-traffic assertion below is an
	// empirical fact, not just a reading of the accepted flag.
	time.Sleep(300 * time.Millisecond)
	if total := ollama.total(); total != 0 {
		return fmt.Errorf("explicit-mode observe produced %d Ollama request(s), want 0", total)
	}
	return nil
}

// checkExperimentalAutoBudget proves experimental-auto capture stores
// watcher-tagged rows (visible as "auto" through the real CLI) and enforces
// the 10-new-row/UTC-day budget at admission, with no watcher call made once
// exhausted.
func checkExperimentalAutoBudget(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	var factN atomic.Int64
	ollama.chatResponder = func(user string) ([]string, []string) {
		n := factN.Add(1)
		return []string{fmt.Sprintf("uat fact number %d: user always wants build target %d used", n, n)}, nil
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "experimental-auto")
	if err != nil {
		return err
	}
	proc, err := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, "daemon.log"))
	if err != nil {
		return err
	}
	defer proc.stop(5 * time.Second)
	if err := waitRPCReady(ctx, port, 10*time.Second); err != nil {
		return err
	}

	for i := 1; i <= memWatcherDailyBudgetUAT; i++ {
		msg := fmt.Sprintf("remember this for me: my favorite build target is target-%d, always use it", i)
		res, err := rpcCall(ctx, port, "observe", map[string]any{"user": msg})
		if err != nil {
			return fmt.Errorf("observe #%d: %w", i, err)
		}
		if accepted, _ := res["accepted"].(bool); !accepted {
			return fmt.Errorf("observe #%d rejected while the budget should still be open: %#v", i, res)
		}
		if err := waitForStatCount(ctx, port, "active", i, 5*time.Second); err != nil {
			return fmt.Errorf("observe #%d: capture never landed: %w", i, err)
		}
	}

	chatBefore := ollama.chatCount()
	res, err := rpcCall(ctx, port, "observe", map[string]any{"user": "remember this for me too: eleventh preference, always use it"})
	if err != nil {
		return fmt.Errorf("observe #%d: %w", memWatcherDailyBudgetUAT+1, err)
	}
	if accepted, _ := res["accepted"].(bool); accepted {
		return fmt.Errorf("observe #%d accepted; the daily budget of %d should already be exhausted", memWatcherDailyBudgetUAT+1, memWatcherDailyBudgetUAT)
	}
	reason, _ := res["reason"].(string)
	if !strings.Contains(strings.ToLower(reason), "budget") {
		return fmt.Errorf("rejection reason %q does not mention the budget", reason)
	}
	time.Sleep(300 * time.Millisecond)
	if after := ollama.chatCount(); after != chatBefore {
		return fmt.Errorf("the over-budget observe still reached the watcher (chat calls %d -> %d)", chatBefore, after)
	}

	recallRes, err := rpcCall(ctx, port, "recall", map[string]any{"query": "*", "limit": 50})
	if err != nil {
		return fmt.Errorf("recall '*': %w", err)
	}
	hits, _ := recallRes["hits"].([]any)
	watcherHits := 0
	for _, h := range hits {
		hm, _ := h.(map[string]any)
		if hm["source"] == "watcher" {
			watcherHits++
		}
	}
	fmt.Fprintf(lw, "recall '*' returned %d hit(s), %d watcher-sourced\n", len(hits), watcherHits)
	if watcherHits != memWatcherDailyBudgetUAT {
		return fmt.Errorf("recall shows %d watcher-sourced row(s), want exactly %d", watcherHits, memWatcherDailyBudgetUAT)
	}

	// Prove the human-facing CLI actually renders the "auto" tag, through the
	// real candidate binary — not just that the raw RPC's source field is
	// "watcher".
	cliCmd := exec.CommandContext(ctx, pixBin, "memory", "recall", "*", "--limit", "50")
	cliCmd.Env = env
	cliCmd.Dir = phaseDir
	out, err := cliCmd.CombinedOutput()
	fmt.Fprintf(lw, "candidate CLI recall output:\n%s\n", out)
	if err != nil {
		return fmt.Errorf("candidate CLI recall failed: %w", err)
	}
	if strings.Count(string(out), "auto") < memWatcherDailyBudgetUAT {
		return fmt.Errorf("candidate CLI recall output does not render 'auto' for all %d watcher row(s)", memWatcherDailyBudgetUAT)
	}
	return nil
}

// checkRememberSourceSpoof proves an external `remember` call cannot claim
// source="watcher" for itself; the store normalizes it to "unknown".
func checkRememberSourceSpoof(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "explicit")
	if err != nil {
		return err
	}
	proc, err := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, "daemon.log"))
	if err != nil {
		return err
	}
	defer proc.stop(5 * time.Second)
	if err := waitRPCReady(ctx, port, 10*time.Second); err != nil {
		return err
	}

	content := "external caller claims this row came from the watcher"
	res, err := rpcCall(ctx, port, "remember", map[string]any{"content": content, "source": "watcher"})
	if err != nil {
		return fmt.Errorf("remember: %w", err)
	}
	fmt.Fprintf(lw, "remember result: %#v\n", res)
	if id, _ := res["id"].(string); id == "" {
		return fmt.Errorf("remember did not return an id")
	}

	recallRes, err := rpcCall(ctx, port, "recall", map[string]any{"query": "*", "limit": 20})
	if err != nil {
		return fmt.Errorf("recall: %w", err)
	}
	hits, _ := recallRes["hits"].([]any)
	found := false
	for _, h := range hits {
		hm, _ := h.(map[string]any)
		if hm["content"] == content {
			found = true
			if hm["source"] != "unknown" {
				return fmt.Errorf("external remember with source=watcher stored as source=%v, want unknown", hm["source"])
			}
		}
	}
	if !found {
		return fmt.Errorf("could not find the stored row via recall")
	}
	return nil
}

// checkForgetMissingExit1 proves `pix memory forget <missing>` exits 1 with
// a stderr diagnostic, and that --json keeps stdout parseable even on a miss.
func checkForgetMissingExit1(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "explicit")
	if err != nil {
		return err
	}
	proc, err := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, "daemon.log"))
	if err != nil {
		return err
	}
	defer proc.stop(5 * time.Second)
	if err := waitRPCReady(ctx, port, 10*time.Second); err != nil {
		return err
	}

	run := func(args ...string) (stdout, stderr string, exitCode int) {
		cmd := exec.CommandContext(ctx, pixBin, args...)
		cmd.Env = env
		cmd.Dir = phaseDir
		var o, e bytes.Buffer
		cmd.Stdout = &o
		cmd.Stderr = &e
		_ = cmd.Run()
		exitCode = cmd.ProcessState.ExitCode()
		return o.String(), e.String(), exitCode
	}

	stdout1, stderr1, exit1 := run("memory", "forget", "does-not-exist-12345")
	fmt.Fprintf(lw, "forget (plain): exit=%d stdout=%q stderr=%q\n", exit1, stdout1, stderr1)
	if exit1 != 1 {
		return fmt.Errorf("forget <missing> exited %d, want 1", exit1)
	}
	if strings.TrimSpace(stderr1) == "" {
		return fmt.Errorf("forget <missing> produced no stderr diagnostic")
	}

	stdout2, stderr2, exit2 := run("memory", "forget", "does-not-exist-12345", "--json")
	fmt.Fprintf(lw, "forget --json: exit=%d stdout=%q stderr=%q\n", exit2, stdout2, stderr2)
	if exit2 != 1 {
		return fmt.Errorf("forget <missing> --json exited %d, want 1", exit2)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout2), &parsed); err != nil {
		return fmt.Errorf("forget --json stdout is not parseable JSON: %v (stdout=%q)", err, stdout2)
	}
	if ok, _ := parsed["ok"].(bool); ok {
		return fmt.Errorf("forget --json on a missing id reported ok=true")
	}
	return nil
}

// checkV1Migration proves the v2 schema migration preserves row content and
// count, soft-deletes legacy watcher/unknown-source/perishable rows, stamps
// user_version=2 idempotently on a second open, and rolls back atomically
// (no partial soft-delete, no version bump) when a malformed v1 db makes the
// migration fail partway through.
func checkV1Migration(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()

	goodDB := filepath.Join(phaseDir, "good-v1.db")
	if err := writeV1Fixture(goodDB, false); err != nil {
		return fmt.Errorf("build v1 fixture: %w", err)
	}
	beforeCount, beforeContents, err := inspectMemoriesTable(goodDB)
	if err != nil {
		return fmt.Errorf("inspect fixture before migration: %w", err)
	}

	openDB := func(dbPath, label string, wantUp bool, upTimeout time.Duration) *procHandle {
		port, perr := freePort()
		if perr != nil {
			err = perr
			return nil
		}
		env, eerr := memoryEnv(phaseDir, port, ollama.URL(), "")
		if eerr != nil {
			err = eerr
			return nil
		}
		env = setEnv(env, "MEMORY_DB", dbPath)
		proc, serr := startProcess(ctx, pixHostBin, []string{"memory"}, env, phaseDir, filepath.Join(phaseDir, label+".log"))
		if serr != nil {
			err = serr
			return nil
		}
		up := waitTCP(fmt.Sprintf("127.0.0.1:%d", port), upTimeout)
		if up != wantUp {
			if wantUp {
				err = fmt.Errorf("%s: db did not open within %s (migration should have succeeded)", label, upTimeout)
			} else {
				err = fmt.Errorf("%s: db opened its port; migration should have failed closed", label)
			}
		}
		return proc
	}

	proc := openDB(goodDB, "good-open-1", true, 10*time.Second)
	if err != nil {
		if proc != nil {
			proc.stop(2 * time.Second)
		}
		return err
	}
	proc.stop(5 * time.Second)

	version, count, contents, deletedByID, ierr := inspectMigratedDB(goodDB)
	if ierr != nil {
		return fmt.Errorf("inspect migrated db: %w", ierr)
	}
	fmt.Fprintf(lw, "after first migration: version=%d count=%d\n", version, count)
	if version != 2 {
		return fmt.Errorf("user_version after migration = %d, want 2", version)
	}
	if count != beforeCount {
		return fmt.Errorf("row count changed across migration: %d -> %d", beforeCount, count)
	}
	for id, c := range beforeContents {
		if contents[id] != c {
			return fmt.Errorf("content for %s changed across migration: %q -> %q", id, c, contents[id])
		}
	}
	wantDeleted := map[string]bool{"row-watcher-1": true, "row-unknown-1": true, "row-perishable-1": true}
	for id, deleted := range deletedByID {
		if want := wantDeleted[id]; deleted != want {
			return fmt.Errorf("row %s deleted_at set=%v, want %v", id, deleted, want)
		}
	}

	// Idempotency: opening the now-v2 db again must not change anything further.
	proc2 := openDB(goodDB, "good-open-2", true, 10*time.Second)
	if err != nil {
		if proc2 != nil {
			proc2.stop(2 * time.Second)
		}
		return err
	}
	proc2.stop(5 * time.Second)
	version2, count2, _, deletedByID2, ierr := inspectMigratedDB(goodDB)
	if ierr != nil {
		return ierr
	}
	if version2 != 2 || count2 != count {
		return fmt.Errorf("second open changed state: version %d->%d, count %d->%d", version, version2, count, count2)
	}
	for id, deleted := range deletedByID {
		if deletedByID2[id] != deleted {
			return fmt.Errorf("second open changed deleted_at for %s", id)
		}
	}

	// Malformed fixture: migration must fail and roll back atomically.
	badDB := filepath.Join(phaseDir, "bad-v1.db")
	if err := writeV1Fixture(badDB, true); err != nil {
		return fmt.Errorf("build malformed v1 fixture: %w", err)
	}
	beforeBadVersion, beforeBadCount, beforeBadDeleted, ierr := inspectRollbackState(badDB)
	if ierr != nil {
		return ierr
	}

	proc3 := openDB(badDB, "bad-open", false, 4*time.Second)
	if err != nil {
		if proc3 != nil {
			_, exited := proc3.waitExit(6 * time.Second)
			if !exited {
				proc3.stop(2 * time.Second)
			}
		}
		return err
	}
	waitErr, exited := proc3.waitExit(6 * time.Second)
	if !exited {
		proc3.stop(2 * time.Second)
		return fmt.Errorf("malformed v1 fixture: daemon neither opened its port nor exited within budget")
	}
	fmt.Fprintf(lw, "malformed fixture exited as expected: %v\n", waitErr)

	afterBadVersion, afterBadCount, afterBadDeleted, ierr := inspectRollbackState(badDB)
	if ierr != nil {
		return fmt.Errorf("inspect malformed db after failed migration: %w", ierr)
	}
	if afterBadVersion != beforeBadVersion {
		return fmt.Errorf("malformed fixture user_version changed despite a failed migration: %d -> %d", beforeBadVersion, afterBadVersion)
	}
	if afterBadCount != beforeBadCount {
		return fmt.Errorf("malformed fixture row count changed despite a failed migration: %d -> %d", beforeBadCount, afterBadCount)
	}
	if afterBadDeleted != beforeBadDeleted {
		return fmt.Errorf("malformed fixture soft-deleted %d row(s) despite the migration having failed and rolled back (was %d)", afterBadDeleted, beforeBadDeleted)
	}
	return nil
}

// checkPluginRestartRetainsRow proves the supervised memory unit (`pix-host
// serve`) keeps its stable :MEMORY_PORT listener and persisted rows across a
// killed plugin child: `serve` owns the listener, not the child, so Suture
// restarting the child must be invisible to a caller beyond a transient error.
func checkPluginRestartRetainsRow(ctx context.Context, lw io.Writer, pixBin, pixHostBin, phaseDir string) error {
	ollama := newFakeOllama()
	defer ollama.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := memoryEnv(phaseDir, port, ollama.URL(), "explicit")
	if err != nil {
		return err
	}

	proc, err := startProcess(ctx, pixHostBin, []string{"serve"}, env, phaseDir, filepath.Join(phaseDir, "serve.log"))
	if err != nil {
		return fmt.Errorf("start pix-host serve: %w", err)
	}
	defer proc.stop(5 * time.Second)

	if err := waitRPCReady(ctx, port, 20*time.Second); err != nil {
		return fmt.Errorf("supervised serve never answered identity: %w", err)
	}
	parentPid := proc.cmd.Process.Pid
	if !processAlive(parentPid) {
		return fmt.Errorf("serve parent pid %d not alive right after startup", parentPid)
	}

	content := "plugin-restart marker: " + phaseDir
	remRes, err := rpcCall(ctx, port, "remember", map[string]any{"content": content, "source": "cli"})
	if err != nil {
		return fmt.Errorf("remember before kill: %w", err)
	}
	fmt.Fprintf(lw, "remembered before kill: %#v\n", remRes)

	childPid, err := findPluginMemoryChild(pixHostBin, parentPid)
	if err != nil {
		return fmt.Errorf("locate supervised memory plugin child: %w", err)
	}
	fmt.Fprintf(lw, "found plugin memory child pid=%d (serve parent pid=%d)\n", childPid, parentPid)
	if childPid == parentPid {
		return fmt.Errorf("resolved child pid equals the serve parent pid; refusing to kill the parent")
	}

	if err := killPid(childPid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill plugin memory child: %w", err)
	}

	if err := waitRPCReady(ctx, port, 20*time.Second); err != nil {
		return fmt.Errorf("memory port did not recover after killing the plugin child: %w", err)
	}
	if !processAlive(parentPid) {
		return fmt.Errorf("serve parent pid %d died when only the child was killed", parentPid)
	}

	recallRes, err := rpcCall(ctx, port, "recall", map[string]any{"query": "plugin-restart marker", "limit": 10})
	if err != nil {
		return fmt.Errorf("recall after restart: %w", err)
	}
	hits, _ := recallRes["hits"].([]any)
	found := false
	for _, h := range hits {
		hm, _ := h.(map[string]any)
		if hm["content"] == content {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("row written before the kill is not recallable after the plugin child restarted")
	}
	return nil
}
