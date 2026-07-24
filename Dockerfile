# syntax=docker/dockerfile:1
###############################################################################
# pi-stack — multi-model pi coding agent on a Docker Hardened (DHI) Debian base
#
# Mirrors the conventions sbx expects of a sandbox image (reverse-engineered
# from docker/sandbox-templates:shell):
#   - non-root `agent` user (uid 1000), home /home/agent, workdir .../workspace
#   - NPM_CONFIG_PREFIX=/usr/local/share/npm-global on PATH
#   - NO_PROXY for localhost ranges, BASH_ENV=/etc/sandbox-persistent.sh
###############################################################################
# Pinned for deterministic builds. Bump this to clear pi's "update available"
# nag (pi checks npm at runtime, so a new release always nags until you rebump).
# When bumping, re-check the vendored tui patch still applies (build logs print
# "[apply-tui-bottom-pin] patched" vs an "anchor not found" warning).
ARG PI_PACKAGE=@earendil-works/pi-coding-agent@0.82.0

# Hardened Node, maintained by Docker (DHI). Debian/glibc, so our entire apt
# toolchain (clangd, chromium, gh, ruff, build-essential) keeps working — we just
# stop hand-pinning a Node tarball and let Docker harden + update Node for us.
FROM dhi.io/node:25-debian13-dev

ARG PI_PACKAGE
USER root

# --- system tools (DHI-patched via apt) ---------------------------------------
# The node image ships node+npm+bash+apt but not these. pi needs git + ripgrep;
# gh powers `ship`; hostname + curl are conveniences; ca-certs for TLS.
# `which`: trixie dropped it from debianutils into its own package. Scripts
# should prefer POSIX `command -v`, but some third-party tooling still shells
# out to `which`, so keep it on PATH.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git gh ripgrep hostname gzip curl which \
 && rm -rf /var/lib/apt/lists/*

# Note: Google Workspace access is NOT in this image. It runs host-side as the
# `gog` MCP server, spawned by the sbx gateway and reached through it (the same
# pattern as `slack`). The VM never talks to Google — no CLI, no token service,
# no Google endpoints in the kit allowlist. See `make mcp-register`.

# --- npm global prefix (sandbox-template convention) --------------------------
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=/home/agent/.local/bin:/usr/local/share/npm-global/bin:$PATH
RUN mkdir -p "$NPM_CONFIG_PREFIX"

# --- pi coding agent ----------------------------------------------------------
RUN npm install -g --ignore-scripts "${PI_PACKAGE}" \
 && pi --version

# --- vendored renderer patch: "bottom-block pin" ------------------------------
# pi-tui's doRender() doesn't re-anchor the viewport on a bottom-anchored buffer
# SHRINK, so the input box + powerbar drift up by a row while streaming. There's
# no extension/config fix (the churn is in pi's own chat render), so we patch the
# installed renderer at build time. The script is idempotent and NON-FATAL: if a
# future pi version moves the anchor it warns and leaves the file unpatched
# rather than failing the build. Full writeup: docs/upstream/tui-bottom-pin.md.
COPY scripts/patches/ /usr/local/share/pi-stack/patches/
RUN node /usr/local/share/pi-stack/patches/apply-tui-bottom-pin.mjs

# --- build toolchain (native npm modules + dev typecheck) ---------------------
# build-essential + python3 so native npm modules (node-pty etc.) compile;
# typescript gives `tsc` for type-checking the baked extensions during dev.
# (The clangd + node LSP servers were only here to feed pi-lens inline
# diagnostics; removed with pi-lens.)
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential python3 \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g --ignore-scripts typescript \
 && npm cache clean --force
# ruff (Python lint/format) via official static binary.
# ARG is pinned (like FD_VERSION / GO_VERSION) so builds are reproducible:
# `releases/latest` silently changes on every new ruff release, which could
# introduce linting behaviour changes or build failures without warning (H-4).
ARG RUFF_VERSION=0.15.22
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) rt=aarch64-unknown-linux-gnu ;; \
      amd64) rt=x86_64-unknown-linux-gnu  ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/astral-sh/ruff/releases/download/${RUFF_VERSION}/ruff-${rt}.tar.gz" -o /tmp/ruff.tgz; \
    tar -xzf /tmp/ruff.tgz -C /tmp; \
    mkdir -p /usr/local/bin; \
    install -m0755 "/tmp/ruff-${rt}/ruff" /usr/local/bin/ruff; \
    rm -rf /tmp/ruff.tgz "/tmp/ruff-${rt}"; \
    ruff --version

# --- fd (fast file finder) via official static binary -------------------------
# pi auto-downloads fd to ~/.pi/agent/bin at runtime if it's not on PATH;
# baking it avoids that per-sandbox download. (fd-find is not in the DHI apt.)
ARG FD_VERSION=10.4.2
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) ft=aarch64-unknown-linux-gnu ;; \
      amd64) ft=x86_64-unknown-linux-gnu  ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-${ft}.tar.gz" -o /tmp/fd.tgz; \
    tar -xzf /tmp/fd.tgz -C /tmp; \
    mkdir -p /usr/local/bin; \
    install -m0755 "/tmp/fd-v${FD_VERSION}-${ft}/fd" /usr/local/bin/fd; \
    rm -rf /tmp/fd.tgz "/tmp/fd-v${FD_VERSION}-${ft}"; \
    fd --version

# --- Go toolchain via the official tarball -------------------------------------
# HOST code is Go (services/host -> pi-stack-host); baking the toolchain lets you
# build + test it FROM INSIDE a sandbox when hacking on pi-stack itself. NOT from
# apt: Debian trixie ships golang ~1.24, but services/host/go.mod requires `go
# 1.26`, so an older apt Go would force a per-build GOTOOLCHAIN network download
# (or fail). Pinning the tarball to match go.mod is deterministic and offline
# after the layer is cached. Same rationale as the ruff/fd static binaries above.
ARG GO_VERSION=1.26.5
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) gt=linux-arm64 ;; \
      amd64) gt=linux-amd64 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${gt}.tar.gz" -o /tmp/go.tgz; \
    rm -rf /usr/local/go; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    /usr/local/go/bin/go version
# Put the toolchain on PATH. GOTOOLCHAIN=local pins the build to THIS baked Go so
# a build never silently downloads a toolchain from the network; keep GO_VERSION
# >= the `go` directive in services/host/go.mod (bump both together, like
# PI_PACKAGE). GOPATH/GOCACHE use their $HOME defaults, writable by the agent user
# created below.
ENV PATH=/usr/local/go/bin:$PATH
ENV GOTOOLCHAIN=local

# (Removed: headless browser stack — chromium + agent-browser. The pi
# agent-browser extension can't survive an in-place /reload, and it's the only
# thing that was still wedging it. Occasional browser work goes to Playwright/MCP
# instead. Re-add chromium + pi-agent-browser-native if you want it back.)

# --- non-root agent user (uid 1000, matches stock templates) ------------------
# The hardened base ships no shadow-utils (useradd/groupadd), so create the user
# directly in /etc/passwd + /etc/group rather than pulling in the passwd package.
RUN printf 'agent:x:1000:1000::/home/agent:/bin/bash\n' >> /etc/passwd \
 && printf 'agent:x:1000:\n' >> /etc/group \
 && mkdir -p /home/agent/workspace /home/agent/.pi/agent/bin \
 && ln -sf /usr/local/bin/fd /home/agent/.pi/agent/bin/fd \
 && chown -R 1000:1000 /home/agent "$NPM_CONFIG_PREFIX"

# --- bake the harness (pi auto-discovers ~/.pi/agent/{skills,extensions})
COPY --chown=agent:agent settings.json    /home/agent/.pi/agent/settings.json
COPY --chown=agent:agent keybindings.json  /home/agent/.pi/agent/keybindings.json
# mcp.json registers the sbx Cloud MCP Gateway (atlassian/notion/granola/linear/…).
# The gateway DNS name is stable; lifecycle:lazy means sandboxes without a
# gateway profile attached just never connect it (no eager-connect failure).
COPY --chown=agent:agent mcp.json          /home/agent/.pi/agent/mcp.json
# capabilities.json maps abstract capabilities (chat, docs, github, ...) to concrete
# providers (mcp server / cli / http / none). Swap it to retarget every data skill
# at once. See the capability-routing skill.
COPY --chown=agent:agent capabilities.json /home/agent/.pi/agent/capabilities.json
# routing.json is the model router's compiled intent->model map (same swap-one-file
# pattern as capabilities.json, but for MODELS). Agents declare an `intent:` and
# subagents.ts resolves it here. Regenerate on the host with `pi-stack route
# compile` after editing services/host/routing/scorecard.json. See
# docs/design/routing.md.
COPY --chown=agent:agent routing.json      /home/agent/.pi/agent/routing.json
COPY --chown=agent:agent skills/       /home/agent/.pi/agent/skills/
COPY --chown=agent:agent extensions/   /home/agent/.pi/agent/extensions/
COPY --chown=agent:agent agents/       /home/agent/.pi/agent/agents/
COPY --chown=agent:agent themes/       /home/agent/.pi/agent/themes/
# Note: company tooling (e.g. a `snow` wrapper) is NOT in the public image. Such
# in-sandbox wrappers are delivered by a private overlay mixin kit at run time
# (`--kit ./pi-kit-work`); see docs/OVERLAY.md.

# --- memory (self-learning loop) ----------------------------------------------
# The recall extension baked above is a thin client. The store itself runs on the
# HOST (global, single writer, persistent) and the extension calls it over
# JSON-RPC. Nothing about the store ships in the image; it only needs the URL.
ENV MEMORY_URL=http://host.docker.internal:11435

# --- sandbox runtime conventions ----------------------------------------------
# host.docker.internal bypasses the proxy so the recall extension can reach the
# host memory service directly (recall is skipped if it isn't running).
ENV NO_PROXY=localhost,127.0.0.1,::1,172.17.0.0/16,host.docker.internal \
    no_proxy=localhost,127.0.0.1,::1,172.17.0.0/16,host.docker.internal \
    BASH_ENV=/etc/sandbox-persistent.sh \
    HOME=/home/agent
RUN touch /etc/sandbox-persistent.sh && chmod 0644 /etc/sandbox-persistent.sh

LABEL com.docker.sandboxes="kit" \
      org.opencontainers.image.title="pi-stack" \
      org.opencontainers.image.description="Multi-model pi coding agent on a DHI Debian base"

USER agent

# --- pi harness packages (curated; full-auto, no permission gate) -------------
# subagents (multi-model fan-out) now ship as our OWN first-party extension
# (extensions/subagents.ts, baked via the COPY extensions/ below); the off-the-
# shelf @tintinweb/pi-subagents stays DISABLED — see the notes below.
# plan mode (pi-plan), MCP adapter (wire servers per-project), todo list,
# simplify, web access, usage. (powerbar removed: its session-shutdown handler
# uses a stale ctx after ctx.reload(), which wedges /reload. pi-lens removed: its
# inline LSP diagnostics were not pulling their weight.)
#
# PINNED — these MUST be version-locked, not floating. They peer-depend on
# @earendil-works/pi-ai with "*", so an unpinned `pi install` grabs the latest on
# every rebuild; a newer extension then imports a pi-ai API (e.g. `/compat`) that
# the pinned PI_PACKAGE doesn't ship, and the agent dies at load
# ("Cannot find module '.../pi-ai/dist/index.js/compat'"). These versions are the
# set that was current when the pinned PI_PACKAGE shipped. When you bump
# PI_PACKAGE, re-pin this list to the versions current at that release
# (newest published on/before the release date).
# NOTE: @tintinweb/pi-subagents stays DISABLED (it hung the event loop forever on
# pi 0.80.x; full trace in docs/upstream/pi-subagents-hang-pi-0.80.md). It is
# REPLACED by our own extensions/subagents.ts, which spawns each child as
# `pi --no-extensions -e <self>` (a fully-loaded child re-binds the ollama/memory
# ports the parent holds and deadlocks at startup — the real freeze) with an
# inactivity + wall-clock watchdog so a stuck subagent is killed, not hung. It is
# a plain baked extension (no npm install line), so nothing to add here; see
# docs/design/subagents-extension.md. Do NOT restore the @tintinweb line.
RUN set -eux; for p in \
      pi-plan@0.1.1 pi-mcp-adapter@2.11.0 \
      pi-manage-todo-list@0.4.0 pi-simplify@0.2.3 pi-web-access@0.13.0 \
      @juanibiapina/pi-extension-settings@0.9.1 \
      pi-usage@0.3.0; do \
      pi install "npm:$p"; \
    done; pi list

# `/todos clear` in pi-manage-todo-list 0.4.0 clears only live memory. Persist
# the clear marker so session resume and compaction continuation respect it.
RUN node /usr/local/share/pi-stack/patches/apply-todo-durable-clear.mjs

# Bound the subagent result-wait so a dead subagent can't park the event loop
# (Esc-proof hang). Idempotent + non-fatal. DISABLED alongside pi-subagents above
# (nothing to patch); it also proved insufficient — on 0.80.x the wait isn't the
# `await` this patches, so it never fired. Re-enable with the extension.
# RUN node /usr/local/share/pi-stack/patches/apply-subagent-timeout.mjs

WORKDIR /home/agent/workspace
CMD ["pi"]
