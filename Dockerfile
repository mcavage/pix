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
ARG PI_PACKAGE=@earendil-works/pi-coding-agent@0.80.3

# Hardened Node, maintained by Docker (DHI). Debian/glibc, so our entire apt
# toolchain (clangd, chromium, gh, ruff, build-essential) keeps working — we just
# stop hand-pinning a Node tarball and let Docker harden + update Node for us.
FROM dhi.io/node:25-debian13-dev

ARG PI_PACKAGE
USER root

# --- system tools (DHI-patched via apt) ---------------------------------------
# The node image ships node+npm+bash+apt but not these. pi needs git + ripgrep;
# gh powers `ship`; hostname + curl are conveniences; ca-certs for TLS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git gh ripgrep hostname gzip curl \
 && rm -rf /var/lib/apt/lists/*

# --- Google Workspace CLI -----------------------------------------------------
# `gws` ships a Rust binary via GitHub releases. Install it directly from a
# pinned release instead of using npm postinstall scripts. The wrapper at
# /usr/local/bin/gws fetches a short-lived bearer from a host token service and
# execs the real binary at /usr/local/bin/_gws.
ARG GWS_CLI_VERSION=0.22.5
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) gt=aarch64-unknown-linux-gnu ;; \
      amd64) gt=x86_64-unknown-linux-gnu  ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/googleworkspace/cli/releases/download/v${GWS_CLI_VERSION}/google-workspace-cli-${gt}.tar.gz" -o /tmp/gws.tgz; \
    mkdir -p /tmp/gws; \
    tar -xzf /tmp/gws.tgz -C /tmp/gws; \
    mkdir -p /usr/local/bin; \
    install -m0755 /tmp/gws/gws /usr/local/bin/_gws; \
    rm -rf /tmp/gws /tmp/gws.tgz; \
    /usr/local/bin/_gws --version

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

# --- language servers / dev tooling (pi-lens inline diagnostics) --------------
# clangd (C/C++ LSP) + a C/C++ build toolchain + python3, so C/C++ projects and
# native npm modules (node-pty etc.) compile. (Java/Go/Rust omitted — add later.)
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      clangd build-essential python3 \
 && rm -rf /var/lib/apt/lists/*
# Node-based LSPs: TS/JS, Python (pyright), YAML, JSON/HTML/CSS/ESLint, Bash.
RUN npm install -g --ignore-scripts \
      typescript typescript-language-server \
      pyright yaml-language-server vscode-langservers-extracted bash-language-server \
 && npm cache clean --force
# ruff (Python lint/format) via official static binary.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) rt=aarch64-unknown-linux-gnu ;; \
      amd64) rt=x86_64-unknown-linux-gnu  ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/astral-sh/ruff/releases/latest/download/ruff-${rt}.tar.gz" -o /tmp/ruff.tgz; \
    tar -xzf /tmp/ruff.tgz -C /tmp; \
    mkdir -p /usr/local/bin; \
    install -m0755 "/tmp/ruff-${rt}/ruff" /usr/local/bin/ruff; \
    rm -rf /tmp/ruff.tgz "/tmp/ruff-${rt}"; \
    ruff --version

# --- fd (fast file finder) via official static binary -------------------------
# pi/pi-lens auto-download fd to ~/.pi/agent/bin at runtime if it's not on PATH;
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

# --- bake the harness (pi auto-discovers ~/.pi/agent/{skills,prompts,extensions})
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
COPY --chown=agent:agent skills/       /home/agent/.pi/agent/skills/
COPY --chown=agent:agent prompts/      /home/agent/.pi/agent/prompts/
COPY --chown=agent:agent extensions/   /home/agent/.pi/agent/extensions/
COPY --chown=agent:agent agents/       /home/agent/.pi/agent/agents/
COPY --chown=agent:agent themes/       /home/agent/.pi/agent/themes/
COPY bin/gws /usr/local/bin/gws
RUN chmod 0755 /usr/local/bin/gws
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
# subagents (multi-model fan-out; driven by our ~/.pi/agent/agents presets),
# plan mode (pi-plan), MCP adapter (wire servers per-project), todo list,
# simplify, web access, pi-lens (LSP diagnostics), usage. (powerbar removed: its
# session-shutdown handler uses a stale ctx after ctx.reload(), which wedges /reload.)
#
# PINNED — these MUST be version-locked, not floating. They peer-depend on
# @earendil-works/pi-ai with "*", so an unpinned `pi install` grabs the latest on
# every rebuild; a newer extension then imports a pi-ai API (e.g. `/compat`) that
# the pinned PI_PACKAGE doesn't ship, and the agent dies at load
# ("Cannot find module '.../pi-ai/dist/index.js/compat'"). These versions are the
# set that was current when PI_PACKAGE (0.80.3, 2026-06-30) shipped. When you
# bump PI_PACKAGE, re-pin this list to the versions current at that release
# (newest published on/before the release date).
RUN set -eux; for p in \
      @tintinweb/pi-subagents@0.13.0 pi-plan@0.1.1 pi-mcp-adapter@2.10.0 \
      pi-manage-todo-list@0.4.0 pi-simplify@0.2.2 pi-web-access@0.13.0 pi-lens@3.8.62 \
      @juanibiapina/pi-extension-settings@0.8.0 \
      pi-usage@0.2.1; do \
      pi install "npm:$p"; \
    done; pi list

WORKDIR /home/agent/workspace
CMD ["pi"]
