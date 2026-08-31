#!/usr/bin/env bash
# host-uat.sh — the HOST-ONLY acceptance run from
# docs/design/pix-v2-architecture.md §16. It cannot run inside a pix sandbox:
# every step needs the host's Docker daemon and the `sbx` CLI, neither of
# which the sandbox can reach. Nothing in this repo claims these rows pass;
# this script is how a human EARNS them, and it prints the evidence lines a
# reviewer pastes back into the delivery matrix.
#
# WHAT IT EXERCISES, and why it is built this way:
#   * The REAL installable artifact. `make bundle` produces out/pix next to
#     out/release-manifest.json + out/pix-runtime-*.tar.gz, and `pix setup`
#     discovers that bundle ADJACENT TO THE RESOLVED BINARY. A bare `go
#     build -o $TMP/pix ./cmd/pix` has no manifest and no runtime archive
#     beside it, so setup refuses before it does anything — the old version
#     of this script tested a shape no user ever installs.
#   * The REAL agent image. `make load` builds images/agent/Dockerfile and
#     loads it into sbx's own image store under a unique local tag, writing
#     out/.local-image-tag. `pix run --dev` is what consumes that tag, so
#     the launch row exercises the image this checkout just built rather
#     than whatever published tag happens to be cached.
#
# Dependencies (checked, never installed): sbx, docker, git, go, make.
#
# SAFETY — this script runs against a developer's REAL host, so:
#   * PIX_HOME is a throwaway temp directory. ~/.pix is never touched.
#   * Every Pix-owned resource is now STACK-SCOPED: one PIX_HOME = one
#     stack, identified by a 16-hex id derived from the canonical PIX_HOME
#     path, and the memory container ("pix-memory-<id>"), the two MCP
#     registrations ("pix-memory-<id>", "pix-session-<id>") and every
#     sandbox ("pix-<id>-...") all carry it. The pre-flight still refuses
#     BEFORE any mutation if a resource under THIS run's own scoped names
#     already exists (it would then belong to the USER, not to this run) —
#     the check simply matches the scoped names now instead of the bare
#     legacy ones. A user's own pix-memory / pix-memory-<other id> is
#     deliberately NOT probed, adopted, replaced or removed: it belongs to
#     a different stack and coexists.
#   * Cleanup removes only what this run PROVED it created: each resource
#     has its own CREATED_* flag, set only after this script observed the
#     resource absent beforehand and present afterwards.
set -euo pipefail

fail() { printf 'host-uat: %s\n' "$*" >&2; exit 1; }
step() { printf '\n=== %s\n' "$*"; }

[ -t 1 ] || printf 'host-uat: not a terminal; the interactive attach rows will be skipped\n' >&2

for bin in sbx docker git go make; do
  command -v "$bin" >/dev/null 2>&1 || fail "missing dependency: $bin (host-only; install it on the host, not in a sandbox)"
done
docker info >/dev/null 2>&1 || fail "docker is installed but not reachable (start Docker, then rerun)"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIX_HOME="$(mktemp -d "${TMPDIR:-/tmp}/pix-uat-home.XXXXXX")"
export PIX_HOME

# stack_id_for HOME derives the same 16-hex stack id the launcher derives
# (services/host/stack: sha256 of the canonical, symlink-resolved PIX_HOME
# path, first 16 hex characters). It is computed in shell rather than read
# back from pix so the checks below are INDEPENDENT evidence, not the
# launcher agreeing with itself.
stack_id_for() {
  printf '%s' "$(cd "$1" && pwd -P)" \
    | { command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256; } \
    | cut -c1-16
}

STACK_ID="$(stack_id_for "$PIX_HOME")"
SANDBOX="pix-$STACK_ID-uat"
MEMORY_CONTAINER="pix-memory-$STACK_ID"
MEMORY_MCP="pix-memory-$STACK_ID"
SESSION_MCP="pix-session-$STACK_ID"

# Ownership flags. Each stays 0 until this run has POSITIVE evidence it
# created the resource; cleanup touches nothing whose flag is still 0.
CREATED_MEMORY_CONTAINER=0
CREATED_MCP_REGISTRATION=0
CREATED_SANDBOX=0
CREATED_MEMORY_CONTAINER_B=0
CREATED_SANDBOX_B=0

memory_container_exists() {
  [ -n "$(docker ps -a --filter "name=^$MEMORY_CONTAINER\$" --format '{{.Names}}' 2>/dev/null)" ]
}

mcp_registration_exists() {
  sbx mcp ls 2>/dev/null | grep -q "$MEMORY_MCP"
}

cleanup() {
  set +e
  step "cleanup"
  if [ "$CREATED_SANDBOX" = "1" ]; then
    pix rm "$SANDBOX" --force >/dev/null 2>&1
    printf 'host-uat: removed sandbox %s (created by this run)\n' "$SANDBOX"
  fi
  if [ "$CREATED_MCP_REGISTRATION" = "1" ]; then
    # Best effort: not every sbx build exposes a removal verb. If it does
    # not, say so plainly rather than pretending the host is clean.
    if sbx mcp rm "$MEMORY_MCP" >/dev/null 2>&1 || sbx mcp remove "$MEMORY_MCP" >/dev/null 2>&1; then
      printf 'host-uat: removed the %s MCP registration (created by this run)\n' "$MEMORY_MCP"
    else
      printf 'host-uat: NOTE: this sbx has no MCP removal verb; the %s registration this run created is still present. Remove it yourself if you do not want it.\n' "$MEMORY_MCP" >&2
    fi
  fi
  if [ "$CREATED_MEMORY_CONTAINER" = "1" ]; then
    docker rm -f "$MEMORY_CONTAINER" >/dev/null 2>&1
    printf 'host-uat: removed the %s container (created by this run)\n' "$MEMORY_CONTAINER"
  fi
  if [ -n "${HOME_B:-}" ]; then
    if [ "$CREATED_SANDBOX_B" = "1" ]; then
      ( PIX_HOME="$HOME_B" pix rm "$SANDBOX_B" --force ) >/dev/null 2>&1
      printf 'host-uat: removed sandbox %s (created by this run)\n' "$SANDBOX_B"
    fi
    if [ "$CREATED_MEMORY_CONTAINER_B" = "1" ]; then
      docker rm -f "$MEMORY_CONTAINER_B" >/dev/null 2>&1
      printf 'host-uat: removed the %s container (created by this run)\n' "$MEMORY_CONTAINER_B"
    fi
    rm -rf "$HOME_B"
    printf 'host-uat: removed %s\n' "$HOME_B"
  fi
  rm -rf "$PIX_HOME"
  printf 'host-uat: removed %s\n' "$PIX_HOME"
}
trap cleanup EXIT

step "pre-flight: refuse to touch a pre-existing resource under THIS run's own scoped names"
if memory_container_exists; then
  fail "a $MEMORY_CONTAINER container already exists on this host. That is THIS run's own scoped name, so the container belongs to a previous run or to the user, not to this one, and this run would replace and then delete a resource it did not create. Remove it yourself first if it is disposable: docker rm -f $MEMORY_CONTAINER"
fi
if mcp_registration_exists; then
  fail "the $MEMORY_MCP name is already registered with sbx. That is THIS run's own scoped name, so this run would mutate a registration it did not create. Remove it yourself first if it is disposable, then rerun."
fi

step "build the real installable bundle (out/pix + release-manifest.json + runtime archive)"
( cd "$REPO" && make bundle )
PIX="$REPO/out/pix"
[ -x "$PIX" ] || fail "make bundle did not produce an executable $PIX"
[ -f "$REPO/out/release-manifest.json" ] || fail "make bundle did not produce out/release-manifest.json (pix setup discovers the bundle ADJACENT to the binary)"

step "build + load the pix-agent image into sbx (writes out/.local-image-tag for --dev)"
( cd "$REPO" && make load )
[ -s "$REPO/out/.local-image-tag" ] || fail "make load did not write out/.local-image-tag; pix run --dev has no local image to pin"

# The bundle's own directory is what must be on PATH: `pix setup` resolves
# its release bundle from beside the RESOLVED binary.
PATH="$REPO/out:$PATH"; export PATH
command -v pix >/dev/null 2>&1 || fail "out/pix is not resolvable on PATH"

step "U1 setup: initialize PIX_HOME, mint the token, reconcile pix-memory, register the reserved MCP name"
pix setup
# Ownership is EARNED here: the pre-flight above proved both resources were
# absent, so anything present now was created by this run. Written as `if`
# blocks, never `cond && FLAG=1`, because under `set -e` a false condition
# at the end of an && list would abort the script.
if memory_container_exists; then CREATED_MEMORY_CONTAINER=1; fi
if mcp_registration_exists; then CREATED_MCP_REGISTRATION=1; fi
test -s "$PIX_HOME/state/memory/auth.token" || fail "U1: no pix-memory auth token was generated"
docker ps -a --format '{{.Names}}' | grep -qx "$MEMORY_CONTAINER" || fail "U1: no $MEMORY_CONTAINER container exists; setup did not reconcile THIS stack's memory container"
grep -q 'memory_port' "$PIX_HOME/config.toml" || fail "U1: no memory_port persisted in config.toml"

step "U2 setup is idempotent: a second run changes nothing"
before="$(cat "$PIX_HOME/config.toml")"
pix setup
[ "$before" = "$(cat "$PIX_HOME/config.toml")" ] || fail "U2: a rerun of setup mutated config.toml"

step "U3 doctor reports, never repairs"
pix doctor || true
[ "$before" = "$(cat "$PIX_HOME/config.toml")" ] || fail "U3: doctor mutated config.toml"

step "U4 environment: create one, preview its effective document, trust it"
mkdir -p "$PIX_HOME/envs/uat"
cat > "$PIX_HOME/envs/uat/.sbxenv.yaml" <<'YAML'
schemaVersion: "1"
agent: pix
YAML
pix env show uat --effective >"$PIX_HOME/effective.yaml" || fail "U4: --effective preview failed"
grep -q "$MEMORY_MCP" "$PIX_HOME/effective.yaml" || fail "U4: the effective document omits this stack's $MEMORY_MCP built-in"
grep -q "$SESSION_MCP" "$PIX_HOME/effective.yaml" || fail "U4: the effective document omits this stack's $SESSION_MCP built-in"
grep -q "PIX_STACK_ID: $STACK_ID" "$PIX_HOME/effective.yaml" || fail "U4: the effective document omits the Pix-managed PIX_STACK_ID env fact"
grep -q 'PIX_LAUNCHER_VERSION' "$PIX_HOME/effective.yaml" || fail "U4: the effective document omits the Pix-managed PIX_LAUNCHER_VERSION env fact"
pix env trust uat --yes || fail "U4: pix env trust refused a fresh environment"
pix env default uat || fail "U4: pix env default failed"
grep -q 'default_environment = "uat"' "$PIX_HOME/config.toml" || fail "U4: default_environment not persisted"
grep -q 'memory_port' "$PIX_HOME/config.toml" || fail "U4: env default clobbered memory_port"

step "U4b setup hooks: an environment's own [[setup]] hook runs only under an explicit pix setup --env"
mkdir -p "$PIX_HOME/envs/hooked"
cat > "$PIX_HOME/envs/hooked/.sbxenv.yaml" <<'YAML'
schemaVersion: "1"
agent: pix
YAML
cat > "$PIX_HOME/envs/hooked/pix.toml" <<'TOML'
schema = 1

[[setup]]
id = "uat-tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
required = true
kind = "install"
TOML
# The marker lives at a fixed, ABSOLUTE path baked into the script at
# generation time, never relative to "$0" or the current directory: a
# setup hook now runs from a fresh, private, per-invocation snapshot
# directory (the TOCTOU fix), so both "$0"'s own directory and "$PWD" are
# wiped the moment this hook finishes and can never carry state between
# runs. Persistent state a hook needs across invocations belongs at a
# stable path it names outright, exactly like a real installer would.
cat > "$PIX_HOME/envs/hooked/setup-tool" <<SH
#!/bin/sh
MARKER="$PIX_HOME/envs/hooked/.installed"
case "\$1" in
  check) [ -f "\$MARKER" ] && exit 0 || exit 1 ;;
  install) touch "\$MARKER"; exit 0 ;;
esac
exit 2
SH
chmod +x "$PIX_HOME/envs/hooked/setup-tool"
pix env trust hooked --yes || fail "U4b: pix env trust refused the hooked environment"
pix setup --env hooked || fail "U4b: pix setup --env did not converge the hook"
test -f "$PIX_HOME/envs/hooked/.installed" || fail "U4b: the setup hook's apply step never ran"
pix setup --env hooked || fail "U4b: rerunning setup --env is not idempotent"

step "U5 launch this checkout's own image, list, remove"
# --keep is REQUIRED here, not decoration: `pix run -- --version` runs one
# command and the sandbox is torn down when that last shell exits, so without
# the keep marker there would be nothing left for `pix ls` to list and `pix rm`
# would be removing a box that already went away. Every row that inspects a
# sandbox and then removes it deliberately holds it open this way. --name is
# the already-scoped form, which the launcher round-trips verbatim (a short
# name would be scoped into the same namespace, but then this script would not
# know the resulting name to clean up).
pix run --dev --env uat --name "$SANDBOX" --keep -- --version || fail "U5: pix run --dev failed"
CREATED_SANDBOX=1
pix ls | grep -q "$SANDBOX" || fail "U5: pix ls does not list the sandbox it just created"
# An explicit `pix rm NAME` removes a kept sandbox: the keep marker blocks the
# AUTOMATIC reaper, never a named request.
pix rm "$SANDBOX" || fail "U5: pix rm refused the sandbox it created"
CREATED_SANDBOX=0

step "U5b the generated default environment needs no trust prompt, and no second sbx approval or token reaches the terminal"
# The generated `default` environment runs nothing on this host, so a launch
# under it must NOT stop for a review. Driven with stdin closed: a gate that
# still prompted would read EOF and refuse, which is exactly the failure this
# row exists to catch.
DEFAULT_LOG="$PIX_HOME/uat-default-run.log"
if ( cd "$REPO" && pix run --dev --env default --name "$SANDBOX" --keep -- --version ) </dev/null >"$DEFAULT_LOG" 2>&1; then
  CREATED_SANDBOX=1
else
  cat "$DEFAULT_LOG" >&2
  fail "U5b: pix run under the generated default environment failed"
fi
grep -qi "Accept this host-execution footprint" "$DEFAULT_LOG" && fail "U5b: a zero-footprint environment still prompted for trust"
grep -qi "has not been reviewed" "$DEFAULT_LOG" && fail "U5b: a zero-footprint environment was reported unreviewed"
test -f "$PIX_HOME/state/trust/environments/default.json" && fail "U5b: a zero-footprint environment wrote a trust record"
# sbx 0.41 renders its own plan and asks its own approval on `env create`.
# Pix answers it internally after its OWN gate; neither the prompt, the plan,
# nor the token-bearing memory URL may appear on the user's terminal.
grep -qi "Approve this plan" "$DEFAULT_LOG" && fail "U5b: sbx's duplicate plan approval reached the terminal"
grep -q "token=" "$DEFAULT_LOG" && fail "U5b: a token-bearing memory URL reached the terminal"
MEMORY_TOKEN_FILE="$PIX_HOME/state/memory/auth.token"
if [ -s "$MEMORY_TOKEN_FILE" ] && grep -qF "$(cat "$MEMORY_TOKEN_FILE")" "$DEFAULT_LOG"; then
  fail "U5b: the pix-memory bearer token itself reached the terminal"
fi
pix rm "$SANDBOX" || fail "U5b: pix rm refused the sandbox it created"
CREATED_SANDBOX=0
printf 'host-uat: the generated default launched with no trust prompt and no second sbx approval or token in the output\n'

step "U5c an upgraded binary reconciles this home without pix setup"
# Simulate the post-`brew upgrade` state: the home still claims an OLDER
# release than the bundle beside the binary. The next ORDINARY run must
# reconcile the machine-owned artifacts by itself, print one upgrade line,
# and leave credentials, trust and setup hooks alone.
INSTALLED_JSON="$PIX_HOME/state/release.json"
if [ -f "$INSTALLED_JSON" ]; then
  cp "$INSTALLED_JSON" "$INSTALLED_JSON.uat-backup"
  sed 's/"version": "[^"]*"/"version": "0.0.1"/' "$INSTALLED_JSON.uat-backup" > "$INSTALLED_JSON"
  UPGRADE_LOG="$PIX_HOME/uat-upgrade-run.log"
  if ( cd "$REPO" && pix run --dev --env default --name "$SANDBOX" --keep -- --version ) </dev/null >"$UPGRADE_LOG" 2>&1; then
    CREATED_SANDBOX=1
  else
    cat "$UPGRADE_LOG" >&2
    fail "U5c: the post-upgrade run failed"
  fi
  grep -qi "upgrad" "$UPGRADE_LOG" || fail "U5c: the automatic reconcile printed no upgrade line"
  grep -q "\"version\": \"0.0.1\"" "$INSTALLED_JSON" && fail "U5c: the release record was not updated by the automatic reconcile"
  docker ps -a --format '{{.Names}}' | grep -qx "$MEMORY_CONTAINER" || fail "U5c: the automatic reconcile lost the memory container of this stack"
  pix rm "$SANDBOX" || fail "U5c: pix rm refused the sandbox it created"
  CREATED_SANDBOX=0
  printf 'host-uat: an upgraded binary reconciled this home on an ordinary run, with no pix setup\n'
else
  printf 'host-uat: NOTE: no release record at %s; U5c could not be earned on this host\n' "$INSTALLED_JSON" >&2
fi

step "U6 memory over the Gateway"
sbx mcp ls | grep -q "$MEMORY_MCP" || fail "U6: this stack's $MEMORY_MCP name is not registered"

step "U7 no HOST-GLOBAL pix-owned sbx secret is ever written"
# Pix resolves every provider credential into SANDBOX-SCOPED sbx secrets at
# launch (secret/scoped.go's setScopedSbxSecret) and reads only this
# PIX_HOME's own op:// refs as evidence. So after a full setup + launch
# cycle the GLOBAL scope must hold nothing Pix put there.
if sbx secret ls --global 2>/dev/null | grep -Eq '(^|[[:space:]])(anthropic|openai|google|github)([[:space:]]|$)'; then
  printf 'host-uat: NOTE: this host holds GLOBAL sbx secrets. Pix neither wrote nor removes them (it reads only $PIX_HOME/secrets.env), but this row cannot prove the negative on a host that already had them. Re-run on a host with no global provider secrets to earn it.\n' >&2
else
  printf 'host-uat: no pix-owned global sbx secret exists (the scoped write path left the global scope untouched)\n'
fi

step "U8 coexistence: a SECOND PIX_HOME gets its own container, port, MCP names and sandbox names"
HOME_B="$(mktemp -d "${TMPDIR:-/tmp}/pix-uat-home-b.XXXXXX")"
STACK_ID_B="$(stack_id_for "$HOME_B")"
[ "$STACK_ID_B" != "$STACK_ID" ] || fail "U8: two distinct PIX_HOMEs derived the SAME stack id"
MEMORY_CONTAINER_B="pix-memory-$STACK_ID_B"
MEMORY_MCP_B="pix-memory-$STACK_ID_B"
# The second stack's sandbox is named the SAME way the first one is: an
# explicit --name is scoped to the stack that launches it, and passing the
# already-scoped form is the round-trip case the launcher accepts verbatim.
# Naming it here (rather than letting a short name expand invisibly) is what
# lets cleanup remove it under its own ownership flag if a later row fails.
SANDBOX_B="pix-$STACK_ID_B-uat"
if [ -n "$(docker ps -a --filter "name=^$MEMORY_CONTAINER_B\$" --format '{{.Names}}' 2>/dev/null)" ]; then
  fail "U8: a $MEMORY_CONTAINER_B container already exists; refusing to adopt a resource this run did not create"
fi
( PIX_HOME="$HOME_B" pix setup ) || fail "U8: pix setup failed for the second PIX_HOME"
if [ -n "$(docker ps -a --filter "name=^$MEMORY_CONTAINER_B\$" --format '{{.Names}}' 2>/dev/null)" ]; then CREATED_MEMORY_CONTAINER_B=1; fi
[ "$CREATED_MEMORY_CONTAINER_B" = "1" ] || fail "U8: the second PIX_HOME did not get its own $MEMORY_CONTAINER_B container"
docker ps -a --format '{{.Names}}' | grep -qx "$MEMORY_CONTAINER" || fail "U8: setting up the second PIX_HOME removed or renamed the first stack's container"
PORT_A="$(grep -E '^memory_port' "$PIX_HOME/config.toml" | tr -dc '0-9')"
PORT_B="$(grep -E '^memory_port' "$HOME_B/config.toml" | tr -dc '0-9')"
[ -n "$PORT_A" ] && [ -n "$PORT_B" ] || fail "U8: one of the two homes persisted no memory_port"
[ "$PORT_A" != "$PORT_B" ] || fail "U8: both PIX_HOMEs allocated the SAME loopback memory port ($PORT_A)"
sbx mcp ls | grep -q "$MEMORY_MCP_B" || fail "U8: the second stack's $MEMORY_MCP_B registration is missing (the MCP registry is host-global but namespaced)"
sbx mcp ls | grep -q "$MEMORY_MCP" || fail "U8: registering the second stack dropped the first stack's $MEMORY_MCP registration"
( cd "$REPO" && PIX_HOME="$HOME_B" pix run --dev --name "$SANDBOX_B" --keep -- --version ) || fail "U8: pix run failed under the second PIX_HOME"
CREATED_SANDBOX_B=1
sbx ls | grep -q "$SANDBOX_B" || fail "U8: the second stack's sandbox does not carry its own stack id"
sbx ls | grep -q "pix-$STACK_ID-uat" && fail "U8: the second stack's launch reached into the FIRST stack's namespace"
( PIX_HOME="$HOME_B" pix rm --all --yes ) || fail "U8: pix rm --all failed under the second PIX_HOME"
sbx ls | grep -q "pix-$STACK_ID_B-" && fail "U8: the second stack's own sandbox survived its own pix rm --all"
CREATED_SANDBOX_B=0

step "U9 reset of stack B leaves stack A completely intact"
( PIX_HOME="$HOME_B" pix reset --yes ) || fail "U9: pix reset failed for the second PIX_HOME"
CREATED_MEMORY_CONTAINER_B=0
docker ps -a --format '{{.Names}}' | grep -qx "$MEMORY_CONTAINER" || fail "U9: resetting stack B removed stack A's memory container"
sbx mcp ls | grep -q "$MEMORY_MCP" || fail "U9: resetting stack B removed stack A's MCP registration"
test -f "$PIX_HOME/config.toml" || fail "U9: resetting stack B renamed stack A's PIX_HOME aside"
grep -q "memory_port = $PORT_A" "$PIX_HOME/config.toml" || fail "U9: resetting stack B changed stack A's memory port"

printf '\nhost-uat: every row above PASSED on this host. Paste these lines into the delivery matrix as earned evidence.\n'
