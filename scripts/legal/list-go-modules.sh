#!/usr/bin/env bash
# Print the LIVE set of external Go modules actually reachable from
# services/host's build graph (main module + stdlib excluded), one
# `module@version` per line, sorted+deduped.
#
# This is the ground truth scripts/check-third-party-notices.sh diffs against
# scripts/legal/dependencies.json's `goModules` list — a dependency that shows
# up here but not in the curated ledger is exactly the "fail closed on an
# unreviewed license" case (AC-REL-01).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT/services/host"

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' ./... |
  grep . | sort -u
