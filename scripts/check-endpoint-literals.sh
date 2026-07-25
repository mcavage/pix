#!/usr/bin/env bash
# check-endpoint-literals.sh — one resolver owns the Ollama endpoint.
#
# Every Go readiness path must name the Ollama endpoint through
# effectiveOllamaEndpoint (services/host/cmd/pix/readiness_ollama.go), so
# doctor, status, setup and run can never report on an endpoint the daemon does
# not actually use. This guard fails the build on any 127.0.0.1:11434 or
# localhost:11434 literal in Go source outside the allowlisted files.
#
# Allowlisted, with reasons:
#   readiness_ollama.go  the resolver itself (defaultOllamaHost)
#   memembed.go          the daemon-side resolver on the other side of the RPC
#                        boundary; it reads OLLAMA_HOST itself
#   hostrun.go           OLLAMA_URL handed to the in-sandbox bridge, which is
#                        kit/runtime wiring, not a readiness verdict
#   *_test.go            tests assert the resolver's OUTPUT, which is exactly
#                        the literal this guard protects
#
# Non-Go surfaces (pi-kit/spec.yaml, extensions/ollama-bridge.ts) are out of
# scope: they are kit config and the other side of the sandbox boundary.
#
# Usage: check-endpoint-literals.sh [--self-test]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
allow_re='(readiness_ollama\.go|memembed\.go|hostrun\.go|_test\.go)$'
pattern='(127\.0\.0\.1|localhost):11434'

scan() {
  local dir="$1"
  # -I skips binaries; only tracked Go sources are considered.
  grep -RIn --include='*.go' -E "$pattern" "$dir" 2>/dev/null || true
}

hits=""
while IFS= read -r line; do
  [ -n "$line" ] || continue
  file="${line%%:*}"
  if [[ "$file" =~ $allow_re ]]; then continue; fi
  hits+="$line"$'\n'
done < <(scan "$root/services/host")

if [ -n "$hits" ]; then
  echo "check-endpoint-literals: hard-coded Ollama endpoint outside the resolver:" >&2
  printf '%s' "$hits" >&2
  echo "  fix: resolve it with effectiveOllamaEndpoint(cfg, env) instead" >&2
  exit 1
fi

if [ "${1:-}" = "--self-test" ]; then
  # Prove the guard can actually fail: plant a literal in a scratch tree and
  # confirm the scanner reports it. A guard nobody has seen fail is not a guard.
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  mkdir -p "$tmp/services/host"
  printf 'package x\n\nconst u = "http://127.0.0.1:11434"\n' > "$tmp/services/host/planted.go"
  if [ -z "$(scan "$tmp/services/host")" ]; then
    echo "check-endpoint-literals --self-test: the scanner missed a planted literal" >&2
    exit 1
  fi
  echo "check-endpoint-literals: self-test ok (planted literal detected)"
fi

echo "check-endpoint-literals: ok"
