# syntax=docker/dockerfile:1
###############################################################################
# pix-memory — standalone memory MCP service (docs/design/pix-v2-architecture.md
# §9). Multi-stage: a DHI Go builder produces a static binary, a minimal DHI
# runtime image serves it. Neither stage shares anything with the pix-agent
# image (Dockerfile at the repo root): no shipped skills, no Pi, no Node.
#
# AC-REL-03 (explicit digest/build-arg path, same convention as the root
# Dockerfile): both base images are build ARGs, not hardcoded FROMs, so a
# release build can pin an immutable digest:
#   docker build \
#     --build-arg BUILDER_IMAGE=dhi.io/golang:1.26-debian13-dev@sha256:<digest> \
#     --build-arg RUNTIME_IMAGE=dhi.io/static:latest@sha256:<digest> \
#     -f services/memory/Dockerfile services/memory
# Resolve <digest> against your own DHI-entitled registry session; see
# scripts/release/resolve-base-digest.sh at the repo root. This repo does not,
# and cannot, assert a digest it has not itself resolved against a live,
# credentialed registry pull.
#
# Public fallback (no DHI entitlement): pass public images, e.g.
#   --build-arg BUILDER_IMAGE=docker.io/library/golang:1.26-bookworm
#   --build-arg RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot
# documented ONLY as a substitution path so a non-entitled contributor can
# still build *something* runnable; not a claim that a public image is a
# validated, hardening-equivalent substitute for DHI.
###############################################################################

ARG BUILDER_IMAGE=dhi.io/golang:1.26-debian13-dev
ARG RUNTIME_IMAGE=dhi.io/static:latest

FROM ${BUILDER_IMAGE} AS build
WORKDIR /src

# Cache module downloads separately from source so an app-only change never
# re-fetches the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0: modernc.org/sqlite is pure Go, so the binary stays fully
# static and portable to a distroless/scratch-like runtime with no libc.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build \
      -ldflags "-s -w -X pix-memory/server.Version=${VERSION}" \
      -o /out/pix-memory ./cmd/pix-memory

FROM ${RUNTIME_IMAGE}

# Non-root: DHI's static/nonroot runtime images ship a pre-created uid 65532
# ("nonroot"); pinned numerically so this Dockerfile does not depend on that
# user's exact name across DHI/distroless variants.
USER 65532:65532

ENV MEMORY_DATA_DIR=/data \
    MEMORY_BIND=0.0.0.0 \
    MEMORY_PORT=8080

COPY --from=build --chown=65532:65532 /out/pix-memory /pix-memory

# docs/design/pix-v2-architecture.md §9.1: "mount: ~/.pix/state/memory:/data"
# is the ONLY writable mount this container receives.
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/pix-memory"]
