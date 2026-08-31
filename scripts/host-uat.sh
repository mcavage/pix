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
#   * pix-memory is a single GLOBAL Docker container and pix-memory is a
#     single GLOBAL sbx MCP registration. Neither is namespaced per
#     PIX_HOME, so if one already exists it belongs to the USER, not to
#     this run. The script refuses BEFORE any mutation in that case rather
#     than adopting, replacing, or deleting it.
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
SANDBOX="pix-uat"

# Ownership flags. Each stays 0 until this run has POSITIVE evidence it
# created the resource; cleanup touches nothing whose flag is still 0.
CREATED_MEMORY_CONTAINER=0
CREATED_MCP_REGISTRATION=0
CREATED_SANDBOX=0

memory_container_exists() {
  [ -n "$(docker ps -a --filter 'name=^pix-memory$' --format '{{.Names}}' 2>/dev/null)" ]
}

mcp_registration_exists() {
  sbx mcp ls 2>/dev/null | grep -q 'pix-memory'
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
    if sbx mcp rm pix-memory >/dev/null 2>&1 || sbx mcp remove pix-memory >/dev/null 2>&1; then
      printf 'host-uat: removed the pix-memory MCP registration (created by this run)\n'
    else
      printf 'host-uat: NOTE: this sbx has no MCP removal verb; the pix-memory registration this run created is still present. Remove it yourself if you do not want it.\n' >&2
    fi
  fi
  if [ "$CREATED_MEMORY_CONTAINER" = "1" ]; then
    docker rm -f pix-memory >/dev/null 2>&1
    printf 'host-uat: removed the pix-memory container (created by this run)\n'
  fi
  rm -rf "$PIX_HOME"
  printf 'host-uat: removed %s\n' "$PIX_HOME"
}
trap cleanup EXIT

step "pre-flight: refuse to touch a pre-existing pix-memory the user owns"
if memory_container_exists; then
  fail "a pix-memory container already exists on this host. It is GLOBAL (not per-PIX_HOME), so this run would replace and then delete a resource it did not create. Remove it yourself first if it is disposable: docker rm -f pix-memory"
fi
if mcp_registration_exists; then
  fail "the pix-memory MCP name is already registered with sbx. It is GLOBAL, so this run would mutate a registration it did not create. Remove it yourself first if it is disposable, then rerun."
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
version: 1
workspace:
  path: .
YAML
pix env show uat --effective >"$PIX_HOME/effective.yaml" || fail "U4: --effective preview failed"
grep -q 'pix-memory' "$PIX_HOME/effective.yaml" || fail "U4: the effective document omits the reserved pix-memory built-in"
pix env trust uat --yes || fail "U4: pix env trust refused a fresh environment"
pix env default uat || fail "U4: pix env default failed"
grep -q 'default_environment = "uat"' "$PIX_HOME/config.toml" || fail "U4: default_environment not persisted"
grep -q 'memory_port' "$PIX_HOME/config.toml" || fail "U4: env default clobbered memory_port"

step "U4b setup hooks: an environment's own [[setup]] hook runs only under an explicit pix setup --env"
mkdir -p "$PIX_HOME/envs/hooked"
cat > "$PIX_HOME/envs/hooked/.sbxenv.yaml" <<'YAML'
version: 1
workspace:
  path: .
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
cat > "$PIX_HOME/envs/hooked/setup-tool" <<'SH'
#!/bin/sh
case "$1" in
  check) [ -f "$(dirname "$0")/.installed" ] && exit 0 || exit 1 ;;
  install) touch "$(dirname "$0")/.installed"; exit 0 ;;
esac
exit 2
SH
chmod +x "$PIX_HOME/envs/hooked/setup-tool"
pix env trust hooked --yes || fail "U4b: pix env trust refused the hooked environment"
pix setup --env hooked || fail "U4b: pix setup --env did not converge the hook"
test -f "$PIX_HOME/envs/hooked/.installed" || fail "U4b: the setup hook's apply step never ran"
pix setup --env hooked || fail "U4b: rerunning setup --env is not idempotent"

step "U5 launch this checkout's own image, list, remove"
pix run --dev --env uat --name "$SANDBOX" -- --version || fail "U5: pix run --dev failed"
CREATED_SANDBOX=1
pix ls | grep -q "$SANDBOX" || fail "U5: pix ls does not list the sandbox it just created"
pix rm "$SANDBOX" || fail "U5: pix rm refused the sandbox it created"
CREATED_SANDBOX=0

step "U6 memory over the Gateway"
sbx mcp ls | grep -q 'pix-memory' || fail "U6: the reserved pix-memory name is not registered"

printf '\nhost-uat: every row above PASSED on this host. Paste these lines into the delivery matrix as earned evidence.\n'
