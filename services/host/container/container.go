// Package container is the Docker adapter and reconciliation logic for the
// one named `pix-memory` container docs/design/pix-v2-architecture.md §9.1
// describes: an immutable image digest, `--restart unless-stopped`, a
// loopback-only published port, and exactly `~/.pix/state/memory:/data`
// mounted writable. It is an L1 capability — pure docker-CLI mechanism, no
// setup/doctor/reset orchestration of its own, and no import of a sibling
// capability.
//
// Reconcile is the whole contract docs/design/pix-v2-surface.md §8.1 and
// pix-v2-architecture.md §9.1 ask for: adopt a healthy matching container,
// start a stopped matching one, and replace a mismatched one only after the
// caller has seen (and approved) what changed — never a silent recreate, and
// never a removal of `/data`.
package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// Name is the one container name Pix ever reconciles (pix-v2-surface.md §8.1:
// "pix setup starts one machine-local named container").
const Name = "pix-memory"

// DefaultMemoryPort is the ONE canonical loopback port pix-memory publishes
// on, machine-wide (QA re-review MEDIUM finding: this used to be a literal
// `18080` duplicated in both cmd/pix/homeadapters.go and
// workflow/env/effective.go, with only a doc-comment tripwire holding the
// two copies together). Every caller that needs "the pix-memory port" reads
// it from HERE, container being the lowest layer (L1 capability) that both
// cmd/pix (L4) and workflow/env (L3) may import — so a preview
// (`pix env --effective`) and a real launch/setup can never silently
// disagree again. It is still a single fixed default, not allocated: PRD/
// architecture never asked for a port picker, only for one canonical value
// instead of scattered copies, plus a collision probe (PortAvailable) so an
// occupied port is reported instead of failing deep inside `docker start`.
const DefaultMemoryPort = 18080

// ManagedLabel and FingerprintLabel are the Docker labels Reconcile stamps on
// every container it creates. ManagedLabel is the adoption boundary: Reconcile
// only ever adopts, starts, or replaces a container that carries it — an
// unrelated container someone else named "pix-memory" is never touched.
// FingerprintLabel lets a later reconciliation compare configuration without
// re-deriving one from docker inspect's full, unstable-shaped config.
const (
	ManagedLabel     = "pix.managed"
	FingerprintLabel = "pix.fingerprint"
)

// Spec is the exact container configuration setup wants running. Name is a
// field (not always the Name constant) so a test can reconcile a differently
// named double without colliding with a real pix-memory container on the same
// Docker host.
type Spec struct {
	// Name is the container name; production always passes Name (the const).
	ContainerName string
	// Image is an immutable "repo@sha256:<digest>" reference — the release
	// manifest's pix_memory_digest (architecture §3, §9.1). Reconcile does not
	// validate the digest shape itself; release.Manifest already does that at
	// the point the manifest was read.
	Image string
	// HostPort is the loopback-only published port: 127.0.0.1:<HostPort> ->
	// 8080 (server/server.go's MEMORY_PORT default). 0 means "let Docker
	// allocate one", which Reconcile never chooses on its own — a caller that
	// wants a fixed port passes it.
	HostPort int
	// DataDir is the host directory bind-mounted read-write at /data — and
	// the ONLY writable mount this container ever receives (architecture
	// §9.1, surface §8.1). It is never removed by this package, on any path,
	// including replace.
	DataDir string
	// AuthTokenFile is an optional HOST path to the pix-memory bearer token
	// (security re-review round 1 blocker #1: `docker create --env-file`/`-e`
	// both write the resolved value into the container's own Config.Env,
	// which `docker inspect` exposes in full to anything on this host with
	// inspect access — a bearer secret must never land there). Instead this
	// file is bind-mounted READ-ONLY at AuthTokenMountPath, so the token
	// value never appears in Config.Env, in this host's own `docker create`
	// argv, in a label, or in a log — only the host PATH does, which is not
	// secret. "" omits the mount entirely. DataDir remains the only WRITABLE
	// mount this container ever receives; this one is always :ro.
	AuthTokenFile string
}

// AuthTokenMountPath is the fixed in-container path AuthTokenFile is
// mounted at: pix-memory reads its bearer token from here (see
// services/memory/cmd/pix-memory/main.go), never from an environment
// variable in production.
const AuthTokenMountPath = "/run/secrets/pix-memory-auth"

// containerName returns s.ContainerName, defaulting to Name so a caller that
// only cares about the real container need not repeat the constant.
func (s Spec) containerName() string {
	if strings.TrimSpace(s.ContainerName) != "" {
		return s.ContainerName
	}
	return Name
}

// Fingerprint is a content fingerprint of every fact that decides whether a
// running container still matches Spec: image, published port, and the data
// mount. It deliberately excludes ContainerName (the label lives ON a
// container already named correctly) and is stable across Go struct field
// reordering because it hashes an explicit, ordered string, not a JSON
// encoding of the struct.
func (s Spec) Fingerprint() string {
	parts := []string{
		"image=" + s.Image,
		"port=" + strconv.Itoa(s.HostPort),
		"data=" + s.DataDir,
		// The auth-token-file PATH is part of the create configuration identity
		// (a changed path is a changed container), but its CONTENT (the token
		// value) never is — this fingerprint becomes a Docker label, world-
		// readable by anything with `docker inspect` access, so the secret
		// itself must never be derivable from it.
		"authtokenfile=" + s.AuthTokenFile,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CreateArgs is the exact `docker create` argv for Spec — restart policy,
// loopback publish, the one writable data mount, and both labels. It is a
// method so a test can assert the exact argv a Runner received, matching the
// pattern release.GitInitArgs and pixhome already use for git.
func (s Spec) CreateArgs() []string {
	args := []string{
		"create",
		"--name", s.containerName(),
		"--restart", "unless-stopped",
		"--label", ManagedLabel + "=true",
		"--label", FingerprintLabel + "=" + s.Fingerprint(),
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", s.HostPort),
		"-v", s.DataDir + ":/data",
	}
	if s.AuthTokenFile != "" {
		// :ro — DataDir above stays this container's ONLY writable mount.
		args = append(args, "-v", s.AuthTokenFile+":"+AuthTokenMountPath+":ro")
	}
	return append(args, s.Image)
}

// Runner runs one docker CLI invocation and returns its combined
// stdout+stderr and any error — the same small-interface shape
// pixhome.Runner and release.Runner use for git, so a test supplies a
// deterministic double instead of a real `docker` process ever running under
// `go test`.
type Runner interface {
	Run(args ...string) (string, error)
}

// DefaultRunner is the production Runner: os/exec, nothing else.
var DefaultRunner Runner = execRunner{}

type execRunner struct{}

func (execRunner) Run(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Info is what Inspect parses out of `docker inspect <name>`.
type Info struct {
	Exists  bool
	ID      string
	Running bool
	Image   string
	Labels  map[string]string
}

// Fingerprint returns the container's recorded FingerprintLabel, or "" if it
// carries none (an unmanaged container, or one created before labeling
// existed).
func (i Info) Fingerprint() string { return i.Labels[FingerprintLabel] }

// Managed reports whether i carries ManagedLabel — the adoption boundary
// Reconcile enforces before it will start, stop, or remove anything.
func (i Info) Managed() bool { return i.Labels[ManagedLabel] == "true" }

// dockerInspectState is the subset of `docker inspect`'s JSON this package
// reads. It intentionally decodes only what it needs rather than mirroring
// Docker's full, unstable inspect schema.
type dockerInspectState struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
}

// notFoundMarkers are the substrings `docker inspect` prints on stderr (folded
// into CombinedOutput) for a name Docker has never heard of. Matched
// case-insensitively against the WHOLE combined output, since a Runner double
// or a future Docker release may vary punctuation exactly.
var notFoundMarkers = []string{"no such object", "no such container"}

// Inspect reports whatever `docker inspect <name>` knows about the named
// container. A name Docker does not recognize is reported as Info{Exists:
// false}, not an error — "absent" is this package's ordinary starting state,
// not a failure to check.
func Inspect(runner Runner, name string) (Info, error) {
	if runner == nil {
		runner = DefaultRunner
	}
	out, err := runner.Run("inspect", name)
	if err != nil {
		low := strings.ToLower(out)
		for _, marker := range notFoundMarkers {
			if strings.Contains(low, marker) {
				return Info{Exists: false}, nil
			}
		}
		return Info{}, fmt.Errorf("docker inspect %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	var states []dockerInspectState
	if jerr := json.Unmarshal([]byte(out), &states); jerr != nil {
		return Info{}, fmt.Errorf("parse docker inspect %s: %w", name, jerr)
	}
	if len(states) == 0 {
		return Info{Exists: false}, nil
	}
	s := states[0]
	return Info{
		Exists:  true,
		ID:      s.ID,
		Running: s.State.Running,
		Image:   s.Config.Image,
		Labels:  s.Config.Labels,
	}, nil
}

// Absent reports whether the named container does not exist at all — the
// exact fact `pix reset` must prove before it may rename PIX_HOME
// (architecture §12: "A post-operation probe must confirm the container is
// absent before Pix moves its mounted state directory").
func Absent(runner Runner, name string) (bool, error) {
	info, err := Inspect(runner, name)
	if err != nil {
		return false, err
	}
	return !info.Exists, nil
}

// Stop stops the named container if it is running. Stopping an already-
// stopped or nonexistent container is a no-op, never an error: this mirrors
// `docker stop`'s own idempotence and lets StopAndRemove call it
// unconditionally.
func Stop(runner Runner, name string) error {
	if runner == nil {
		runner = DefaultRunner
	}
	if out, err := runner.Run("stop", name); err != nil {
		low := strings.ToLower(out)
		for _, marker := range notFoundMarkers {
			if strings.Contains(low, marker) {
				return nil
			}
		}
		return fmt.Errorf("docker stop %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Remove removes the named container (stopped or not; -f covers a container
// that raced back to running). It never touches a bind-mounted data
// directory — Docker's own `rm` cannot reach outside the container in the
// first place, but the guarantee is worth stating here since this is the one
// place this package deletes anything.
func Remove(runner Runner, name string) error {
	if runner == nil {
		runner = DefaultRunner
	}
	if out, err := runner.Run("rm", "-f", name); err != nil {
		low := strings.ToLower(out)
		for _, marker := range notFoundMarkers {
			if strings.Contains(low, marker) {
				return nil
			}
		}
		return fmt.Errorf("docker rm %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

// StopAndRemove stops then removes the named container. Used by both
// Reconcile's replace path and `pix reset`.
func StopAndRemove(runner Runner, name string) error {
	if err := Stop(runner, name); err != nil {
		return err
	}
	return Remove(runner, name)
}

// Create issues `docker create` for spec and returns the new container ID.
func Create(runner Runner, spec Spec) (string, error) {
	if runner == nil {
		runner = DefaultRunner
	}
	out, err := runner.Run(spec.CreateArgs()...)
	if err != nil {
		return "", fmt.Errorf("docker create %s: %w (%s)", spec.containerName(), err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// Start issues `docker start` for the named container.
func Start(runner Runner, name string) error {
	if runner == nil {
		runner = DefaultRunner
	}
	if out, err := runner.Run("start", name); err != nil {
		return fmt.Errorf("docker start %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Action names what Reconcile did.
type Action string

const (
	// ActionAdopted: a healthy, matching, already-running container was found
	// and used as-is.
	ActionAdopted Action = "adopted"
	// ActionStarted: a stopped, matching container was started.
	ActionStarted Action = "started"
	// ActionCreated: no container existed; one was created and started.
	ActionCreated Action = "created"
	// ActionReplaced: a mismatched container was stopped, removed (its /data
	// mount untouched), and recreated to match Spec.
	ActionReplaced Action = "replaced"
	// ActionRefusedReplace: a mismatched container was found, but
	// ReconcileOptions.ConfirmReplace declined the replace. The old container
	// is left exactly as it was.
	ActionRefusedReplace Action = "refused-replace"
)

// Result reports what Reconcile did and its final probed state.
type Result struct {
	Action Action
	ID     string
	// PreviousImage and PreviousFingerprint are set on ActionReplaced and
	// ActionRefusedReplace: the "show" half of "show then replace mismatch" —
	// what the OLD container was running, so a caller can render the exact
	// drift before or after the decision.
	PreviousImage       string
	PreviousFingerprint string
	// ProbeErr is set when reconciliation itself succeeded (the container is
	// running the right image) but the post-reconcile readiness probe did
	// not: setup must not report ready in that case. nil on ActionCreated/
	// Started/Adopted means the probe passed.
	ProbeErr error
}

// Prober is the injectable post-reconcile readiness check: "/healthz and an
// MCP initialization/tool-list probe succeed" (architecture §9.1). Reconcile
// calls it only after the container is running the requested Spec — never as
// a substitute for checking Docker's own state first.
type Prober interface {
	// Probe checks the container reachable at baseURL (e.g.
	// "http://127.0.0.1:<port>") and returns a descriptive error the moment
	// any required check fails.
	Probe(baseURL string) error
}

// ReconcileOptions carries the one decision Reconcile cannot make for itself:
// whether a mismatched container may be replaced. A nil ConfirmReplace always
// proceeds (production default: setup shows the drift and asks before ever
// calling Reconcile with this field unset is a caller bug, not this
// package's), which keeps the core loop usable in tests without a UI.
type ReconcileOptions struct {
	// ConfirmReplace is called with the current (mismatched) Info and the
	// wanted Spec before Reconcile stops/removes/recreates. Returning false
	// refuses the replace: Reconcile returns ActionRefusedReplace and leaves
	// the existing container untouched.
	ConfirmReplace func(current Info, want Spec) bool
}

// baseURL renders the loopback endpoint a Prober checks for spec.
func baseURL(spec Spec) string {
	return fmt.Sprintf("http://127.0.0.1:%d", spec.HostPort)
}

// PortInUseError is PortAvailable's refusal: spec.HostPort is already bound
// by something on this host, so creating pix-memory here would either fail
// deep inside `docker start` (opaque) or, worse, appear to succeed while an
// unrelated process actually answers that port.
type PortInUseError struct{ Port int }

func (e *PortInUseError) Error() string {
	return fmt.Sprintf("port 127.0.0.1:%d is already in use by another process on this host; pix-memory needs it free before it can start (stop whatever is using it, then rerun `pix setup`)", e.Port)
}

// dockerPortConflictMarkers are the substrings Docker's OWN daemon prints on
// stderr (folded into CombinedOutput, same as notFoundMarkers above) when a
// `docker create`/`docker start` publish loses a bind race: "Bind for
// 0.0.0.0:<port> failed: port is already allocated" (create) and "...driver
// failed programming external connectivity... address already in use"
// (start). Matched case-insensitively, same as notFoundMarkers.
var dockerPortConflictMarkers = []string{
	"port is already allocated",
	"address already in use",
}

// isDockerPortConflict reports whether out — a docker create/start
// invocation's own combined output — names a bind conflict ON hostPort
// SPECIFICALLY, never a bare marker-substring match against arbitrary
// stderr. Docker's own message always embeds the exact host:port pair it
// failed to bind ("Bind for 0.0.0.0:18080 failed: port is already
// allocated", "...0.0.0.0:18080: bind: address already in use"), so this
// requires the literal port NUMBER to appear alongside a marker — scoping
// the classification to THIS create/start call's own port, never a
// different resource or a different container docker happened to also
// complain about in the same run. Do not weaken this to a bare marker
// match: an operation/container-scoped false positive here would retry a
// genuinely fatal error (an unrelated permission or driver failure) as if
// it were a recoverable port collision.
func isDockerPortConflict(out string, hostPort int) bool {
	low := strings.ToLower(out)
	if !strings.Contains(low, strconv.Itoa(hostPort)) {
		return false
	}
	for _, marker := range dockerPortConflictMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// removeFailedCreate safely removes a container THIS Reconcile call just
// created but could not start: id is the exact ID docker create just
// returned, never the spec's NAME, so a concurrent process's differently-ID'd
// container under the same name can never be reached by this cleanup even if
// one somehow existed. Best-effort: a removal failure is swallowed (logged
// nowhere — Reconcile already has the real error, a port conflict, to
// report) rather than masking the port-conflict error the caller needs to
// see and act on.
func removeFailedCreate(runner Runner, id string) {
	if runner == nil {
		runner = DefaultRunner
	}
	if id == "" {
		return
	}
	_, _ = runner.Run("rm", "-f", id)
}

// PortAvailable probes whether hostPort is free for a NEW bind on
// 127.0.0.1: exactly the address:port pair Docker's own userland proxy
// binds when the container actually starts. It is a best-effort, racy
// check (something else can still grab the port between this probe and
// dockerd's own bind, and Docker itself remains the final arbiter) but
// turns the common same-host "something already listens on this port"
// case into an actionable, pix-owned error instead of an opaque `docker
// start` failure surfacing three layers down.
func PortAvailable(hostPort int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return &PortInUseError{Port: hostPort}
	}
	return ln.Close()
}

// Reconcile is the whole "one named container" contract: adopt a healthy
// matching container, start a stopped matching one, create one when none
// exists, or replace a mismatched one after ConfirmReplace approves — then
// probe before calling any of those outcomes ready.
func Reconcile(runner Runner, spec Spec, prober Prober, opts ReconcileOptions) (Result, error) {
	if runner == nil {
		runner = DefaultRunner
	}
	name := spec.containerName()
	want := spec.Fingerprint()

	info, err := Inspect(runner, name)
	if err != nil {
		return Result{}, err
	}

	switch {
	case !info.Exists:
		if perr := PortAvailable(spec.HostPort); perr != nil {
			return Result{}, perr
		}
		id, err := Create(runner, spec)
		if err != nil {
			if isDockerPortConflict(err.Error(), spec.HostPort) {
				return Result{}, &PortInUseError{Port: spec.HostPort}
			}
			return Result{}, err
		}
		if err := Start(runner, name); err != nil {
			if isDockerPortConflict(err.Error(), spec.HostPort) {
				// Created but never started: remove THIS run's own container
				// (by ID, never by name) so a caller's reallocate-and-retry
				// never collides with, or later adopts, this leftover.
				removeFailedCreate(runner, id)
				return Result{}, &PortInUseError{Port: spec.HostPort}
			}
			return Result{}, err
		}
		res := Result{Action: ActionCreated, ID: id}
		res.ProbeErr = probeIfPresent(prober, spec)
		return res, nil

	case info.Managed() && info.Fingerprint() == want:
		if info.Running {
			res := Result{Action: ActionAdopted, ID: info.ID}
			res.ProbeErr = probeIfPresent(prober, spec)
			return res, nil
		}
		if err := Start(runner, name); err != nil {
			return Result{}, err
		}
		res := Result{Action: ActionStarted, ID: info.ID}
		res.ProbeErr = probeIfPresent(prober, spec)
		return res, nil

	default:
		if opts.ConfirmReplace != nil && !opts.ConfirmReplace(info, spec) {
			return Result{
				Action:              ActionRefusedReplace,
				ID:                  info.ID,
				PreviousImage:       info.Image,
				PreviousFingerprint: info.Fingerprint(),
			}, nil
		}
		if err := StopAndRemove(runner, name); err != nil {
			return Result{}, err
		}
		if perr := PortAvailable(spec.HostPort); perr != nil {
			return Result{}, perr
		}
		id, err := Create(runner, spec)
		if err != nil {
			if isDockerPortConflict(err.Error(), spec.HostPort) {
				return Result{}, &PortInUseError{Port: spec.HostPort}
			}
			return Result{}, err
		}
		if err := Start(runner, name); err != nil {
			if isDockerPortConflict(err.Error(), spec.HostPort) {
				removeFailedCreate(runner, id)
				return Result{}, &PortInUseError{Port: spec.HostPort}
			}
			return Result{}, err
		}
		res := Result{
			Action:              ActionReplaced,
			ID:                  id,
			PreviousImage:       info.Image,
			PreviousFingerprint: info.Fingerprint(),
		}
		res.ProbeErr = probeIfPresent(prober, spec)
		return res, nil
	}
}

func probeIfPresent(prober Prober, spec Spec) error {
	if prober == nil {
		return nil
	}
	return prober.Probe(baseURL(spec))
}

// Ready reports whether res represents a container Reconcile both put in the
// right state AND successfully probed. Setup must gate its own "ready" report
// on this, never on Action alone (architecture §9.1: "reports ready only
// after /healthz and an MCP initialization/tool-list probe succeed").
func (r Result) Ready() bool {
	switch r.Action {
	case ActionCreated, ActionStarted, ActionAdopted, ActionReplaced:
		return r.ProbeErr == nil
	default:
		return false
	}
}
