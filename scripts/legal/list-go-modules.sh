#!/usr/bin/env bash
# Print the LIVE set of external Go modules actually reachable from
# services/host's build graph (main module + stdlib excluded), one
# `module@version` per line, sorted+deduped.
#
# This is the ground truth scripts/check-third-party-notices.sh diffs against
# scripts/legal/dependencies.json's `goModules` list — a dependency that shows
# up here but not in the curated ledger is exactly the "fail closed on an
# unreviewed license" case (AC-REL-01).
#
# MUST cover every GOOS/GOARCH the binary actually ships for, not just the
# host running this script. `go list -deps` only walks the build graph for
# the CURRENT GOOS/GOARCH — a platform-gated import (e.g. modernc.org/libc's
# darwin/windows-only use of github.com/ncruces/go-strftime, invisible on
# linux) is otherwise silently missed here even though
# .github/workflows/publish.yml cross-compiles pix/pix-host for it. Release
# targets are darwin/amd64 + darwin/arm64 (pix's host lifecycle is macOS-only,
# see publish.yml's "Cross-compile both binaries" step); the native dev
# GOOS/GOARCH is included too so a local-only platform quirk is never worse
# than the release set.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT/services/host"

RELEASE_TARGETS="darwin/amd64 darwin/arm64"
NATIVE_TARGET="$(go env GOOS)/$(go env GOARCH)"

targets="$NATIVE_TARGET $RELEASE_TARGETS"
for target in $targets; do
  os="${target%/*}"
  arch="${target#*/}"
  GOOS="$os" GOARCH="$arch" go list -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' ./...
done | grep . | sort -u
