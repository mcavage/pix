#!/usr/bin/env bash
# AC-REL-04 (cross-run tag immutability): is `<image>:<version>` ALREADY in the
# registry?
#
# Why this exists. publish.yml's `version` job picks the next version by
# scanning for an unused `v<version>` GIT TAG, and the git tag is created by the
# `bump` job — AFTER `merge` has already pushed `<image>:<version>`. So a run
# that pushes the image and then fails (or is cancelled) before `bump` leaves
# the git tag unused while the Docker tag exists. The next push to main then
# selects the SAME version and `docker buildx imagetools create` silently
# OVERWRITES the published tag with different bytes. `out/provenance/<v>.json`
# cannot catch that: it is written into a fresh, ephemeral runner workspace, so
# the second run has no prior record to compare against. The registry is the
# only durable cross-run state, so the registry is what we ask.
#
# Verdicts (stdout), exit status:
#   free   / 0   the tag does not exist — safe to publish
#   taken  / 0   the tag exists — the caller must select a new patch, or refuse
#   (none) / 2   UNDECIDED — auth failure, network failure, no docker, an
#                unrecognized error. Fails CLOSED on purpose: "I could not tell"
#                must never be spelled "free", because that is exactly how a
#                published tag gets overwritten.
#
# Usage:
#   tag-availability.sh <image> <version>
#   tag-availability.sh --classify <exit-code> <stderr-file>   # pure, offline
#
# The --classify mode is the whole decision procedure with no Docker and no
# network, so tests can prove the classification (including that an auth error
# is NOT treated as "free") against fixtures.
set -uo pipefail

# classify <exit-code> <stderr-text> -> prints free|taken, or returns 2.
classify() {
	local code="$1" text="$2"
	if [ "$code" = "0" ]; then
		printf 'taken\n'
		return 0
	fi
	# Registry "this reference does not exist" shapes, across docker/buildx and
	# the Docker Hub / OCI distribution error bodies.
	if printf '%s' "$text" | grep -qiE 'manifest unknown|MANIFEST_UNKNOWN|not found|no such manifest|does not exist|unexpected status:? (404|.*404 Not Found)|NAME_UNKNOWN|repository name not known'; then
		# An auth failure can ALSO mention "not found" for a private repo. If
		# anything auth-shaped is present, we do not get to call it free.
		if printf '%s' "$text" | grep -qiE 'unauthorized|authentication required|denied|forbidden|login|credential'; then
			return 2
		fi
		printf 'free\n'
		return 0
	fi
	return 2
}

if [ "${1:-}" = "--classify" ]; then
	CODE="${2:?usage: tag-availability.sh --classify <exit-code> <stderr-file>}"
	FILE="${3:?usage: tag-availability.sh --classify <exit-code> <stderr-file>}"
	if [ ! -f "$FILE" ]; then
		echo "tag-availability: no such stderr file: $FILE" >&2
		exit 2
	fi
	if ! classify "$CODE" "$(cat "$FILE")"; then
		echo "tag-availability: UNDECIDED — could not tell whether the tag exists" >&2
		echo "  docker exit code: $CODE" >&2
		sed 's/^/  | /' "$FILE" >&2
		exit 2
	fi
	exit 0
fi

IMAGE="${1:?usage: tag-availability.sh <image> <version>}"
VERSION="${2:?usage: tag-availability.sh <image> <version>}"

if ! command -v docker >/dev/null 2>&1; then
	echo "tag-availability: docker is not installed — refusing to fabricate a verdict" >&2
	exit 2
fi

ERR="$(mktemp)"
trap 'rm -f "$ERR"' EXIT
docker buildx imagetools inspect "${IMAGE}:${VERSION}" >/dev/null 2>"$ERR"
CODE="$?"

if ! classify "$CODE" "$(cat "$ERR")"; then
	echo "tag-availability: UNDECIDED for ${IMAGE}:${VERSION} — refusing to guess" >&2
	echo "  docker exit code: $CODE" >&2
	sed 's/^/  | /' "$ERR" >&2
	exit 2
fi
