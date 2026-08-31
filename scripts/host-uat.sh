#!/usr/bin/env bash
# host-uat.sh — the HOST-ONLY acceptance run from
# docs/design/pix-v2-architecture.md §16. It cannot run inside a pix sandbox:
# every step needs the host's Docker daemon and the `sbx` CLI, neither of
# which the sandbox can reach. Nothing in this repo claims these rows pass;
# this script is how a human EARNS them, and it prints the evidence lines a
# reviewer pastes back into the delivery matrix.
#
# Dependencies (checked, never installed): sbx, docker, git, go.
# Safety: a throwaway PIX_HOME and a throwaway environment under it, both
# removed by the EXIT trap. It never touches ~/.pix, and the only sandbox it
# creates is named pix-uat (never pix-pix, which `make run` owns).
set -euo pipefail

fail() { printf 'host-uat: %s\n' "$*" >&2; exit 1; }
step() { printf '\n=== %s\n' "$*"; }

[ -t 1 ] || printf 'host-uat: not a terminal; the interactive attach rows will be skipped\n' >&2

for bin in sbx docker git go; do
  command -v "$bin" >/dev/null 2>&1 || fail "missing dependency: $bin (host-only; install it on the host, not in a sandbox)"
done
docker info >/dev/null 2>&1 || fail "docker is installed but not reachable (start Docker, then rerun)"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIX_HOME="$(mktemp -d "${TMPDIR:-/tmp}/pix-uat-home.XXXXXX")"
export PIX_HOME
SANDBOX="pix-uat"

cleanup() {
  set +e
  step "cleanup"
  pix rm "$SANDBOX" --force >/dev/null 2>&1
  docker rm -f pix-memory >/dev/null 2>&1
  rm -rf "$PIX_HOME"
  printf 'host-uat: removed %s and sandbox %s\n' "$PIX_HOME" "$SANDBOX"
}
trap cleanup EXIT

step "build the launcher"
( cd "$REPO/services/host" && go build -o "$PIX_HOME/pix" ./cmd/pix )
PATH="$PIX_HOME:$PATH"; export PATH

step "U1 setup: initialize PIX_HOME, mint the token, reconcile pix-memory, register the reserved MCP name"
pix setup
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

step "U5 launch, list, remove"
pix run --env uat --name "$SANDBOX" -- --version || fail "U5: pix run failed"
pix ls | grep -q "$SANDBOX" || fail "U5: pix ls does not list the sandbox it just created"
pix rm "$SANDBOX" || fail "U5: pix rm refused the sandbox it created"

step "U6 memory over the Gateway"
sbx mcp ls | grep -q 'pix-memory' || fail "U6: the reserved pix-memory name is not registered"

printf '\nhost-uat: every row above PASSED on this host. Paste these lines into the delivery matrix as earned evidence.\n'
