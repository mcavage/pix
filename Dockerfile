# syntax=docker/dockerfile:1
###############################################################################
# pix — multi-model pi coding agent on a Docker Hardened (DHI) Debian base
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
ARG PI_PACKAGE=@earendil-works/pi-coding-agent@0.84.3

# Hardened Node, maintained by Docker (DHI). Debian/glibc, so our entire apt
# toolchain (clangd, chromium, gh, ruff, build-essential) keeps working — we just
# stop hand-pinning a Node tarball and let Docker harden + update Node for us.
#
# AC-REL-03 (explicit digest/build-arg path): BASE_IMAGE is a build ARG, not a
# hardcoded FROM. The default below is the same DHI tag this image has always
# used (a `make load`/CI build is unaffected). To pin an IMMUTABLE base:
#   docker build --build-arg BASE_IMAGE=dhi.io/node:25-debian13-dev@sha256:<digest> .
# Resolve <digest> yourself against your own DHI-entitled registry session —
# see scripts/release/resolve-base-digest.sh, which shells out to
# `docker buildx imagetools inspect` and refuses to guess or fabricate one.
# This repo does not, and cannot, assert a digest it has not itself resolved
# against a live, credentialed registry pull; see docs/legal/FINDINGS.md.
#
# Public fallback (no DHI entitlement): pass a public Debian-based Node image,
# e.g. --build-arg BASE_IMAGE=docker.io/library/node:25-bookworm. This is
# documented ONLY as a substitution path so a non-entitled contributor can
# still build *something* runnable — it is NOT a claim that a public image is
# a validated, hardening-equivalent substitute for DHI, and this repo does not
# assert any right (unresolved or otherwise) to DHI beyond what the operator
# building the image has separately obtained from Docker, Inc.
ARG BASE_IMAGE=dhi.io/node:25-debian13-dev
FROM ${BASE_IMAGE}

ARG PI_PACKAGE
USER root

# --- system tools (DHI-patched via apt) ---------------------------------------
# The node image ships node+npm+bash+apt but not these. pi needs git + ripgrep;
# gh powers `ship`; hostname + curl are conveniences; ca-certs for TLS.
# `which`: trixie dropped it from debianutils into its own package. Scripts
# should prefer POSIX `command -v`, but some third-party tooling still shells
# out to `which`, so keep it on PATH.
#
# `jq` is NOT a convenience: sbx injects its own /usr/local/bin/xdg-open shim at
# runtime, and that shim builds its JSON body with `jq -nc` before POSTing to
# gateway.docker.internal:3128/_sbx/browser-open. On a DHI base with no jq the
# shim dies at that line and silently degrades to its "Open this URL in your
# browser:" fallback, which is exactly the "links are never clickable in the
# sandbox" symptom. The browser-open door itself works (it answers 200); we were
# just failing to knock. `procps` (ps/top) and `file` are the other three tools
# that are reliably missing the moment you try to diagnose anything in-box.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git gh ripgrep hostname gzip curl which \
      jq procps file \
 && rm -rf /var/lib/apt/lists/*

# Tools that respect $BROWSER (and pi's own link-open path) route through the sbx
# shim instead of guessing at a GUI that does not exist in here.
ENV BROWSER=xdg-open

# Note: Google Workspace access is NOT in this image. It runs host-side as the
# `gog` MCP server, spawned by the sbx gateway and reached through it. The VM
# never talks to Google — no CLI, no token service, no Google endpoints in the
# kit allowlist. See `make mcp-register`. (Slack was the other local stdio MCP
# server this pattern covered; it was externalized — W2/U02a, see
# docs/design/slack-setup.md — and no longer ships built into this image.)

# --- npm global prefix (sandbox-template convention) --------------------------
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=/home/agent/.local/bin:/usr/local/share/npm-global/bin:$PATH
RUN mkdir -p "$NPM_CONFIG_PREFIX"

# --- pi coding agent ----------------------------------------------------------
RUN npm install -g --ignore-scripts "${PI_PACKAGE}" \
 && pi --version

# pi runs inside the sandbox, but its terminal belongs to the host launcher.
# Rewrite the shutdown hint to one exact host command instead of printing an
# unusable in-sandbox command and making pix append a second, ambiguous `-c` hint.
ENV PIX_RESUME_COMMAND="pix resume"

# --- vendored pi patches -------------------------------------------------------
COPY scripts/patches/ /usr/local/share/pix/patches/
RUN node /usr/local/share/pix/patches/apply-pix-resume-command.mjs

# --- vendored renderer patch: hide the trusted host-state block ---------------
# The [pix-trusted-host-state] block must be IN the generated onboarding prompt
# (it is the whole mechanism by which host facts reach the fenced agent), but it
# must not be the first thing a new user reads. This strips it from the display
# text and the editor history only; message.content, and therefore what the model
# receives, is untouched.
RUN node /usr/local/share/pix/patches/apply-hide-host-state.mjs

# --- vendored renderer patch: "bottom-block pin" ------------------------------
# pi-tui's doRender() doesn't re-anchor the viewport on a bottom-anchored buffer
# SHRINK, so the input box + powerbar drift up by a row while streaming. There's
# no extension/config fix (the churn is in pi's own chat render), so we patch the
# installed renderer at build time. The script is idempotent and NON-FATAL: if a
# future pi version moves the anchor it warns and leaves the file unpatched
# rather than failing the build. Full writeup: docs/upstream/tui-bottom-pin.md.
RUN node /usr/local/share/pix/patches/apply-tui-bottom-pin.mjs

# --- build toolchain (native npm modules + dev typecheck) ---------------------
# build-essential + python3 so native npm modules (node-pty etc.) compile;
# typescript gives `tsc` for type-checking the baked extensions during dev.
# (The clangd + node LSP servers were only here to feed pi-lens inline
# diagnostics; removed with pi-lens.)
#
# xxd + the python trio are here because agents reach for them constantly and a
# sandbox that lacks them turns a one-liner into an apt detour:
#   xxd           hexdump/patch a binary, eyeball an encoding bug
#   python3-pip   `pip install <anything>`
#   python3-venv  `python3 -m venv` (Debian splits ensurepip out of the stdlib)
#   python3-dev   headers, so a pip package with a C extension actually builds
# All four straight from apt — DHI patches them like everything else here.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential xxd \
      python3 python3-pip python3-venv python3-dev \
 && rm -rf /var/lib/apt/lists/*

# Debian marks the system interpreter "externally managed" (PEP 668), so a bare
# `pip install x` dies with an error telling you to make a venv. That is right for
# a laptop and wrong for a disposable sandbox: the container IS the environment,
# and the agent hitting this mid-task just burns a turn working around it. Opt out
# globally; `python3 -m venv` is still installed for anyone who wants isolation.
ENV PIP_BREAK_SYSTEM_PACKAGES=1
# Pinned like every other baked tool: an unpinned `npm install -g typescript`
# resolves to whatever the registry serves that day, so the version the license
# ledger records (scripts/legal/dependencies.json -> npmGlobal.typescript) would
# be a claim about a build nobody can reproduce. check-third-party-notices.sh's
# --check-npm-pins gate fails closed if this ARG and the ledger ever drift.
ARG TYPESCRIPT_VERSION=5.9.3
RUN npm install -g --ignore-scripts "typescript@${TYPESCRIPT_VERSION}" \
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
# HOST code is Go (services/host -> pix-host); baking the toolchain lets you
# build + test it FROM INSIDE a sandbox when hacking on pix itself. NOT from
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
# Web search: pin Parallel as the default backend. pi-web-access resolves this
# file from ~/.pi (getWebSearchConfigDir: PI_CODING_AGENT_DIR, else
# $XDG_CONFIG_HOME/pi, else ~/.pi), NOT from ~/.pi/agent. Pinning is safe with
# no key: resolveProvider falls through to the first AVAILABLE backend when the
# named one is unavailable, so an unkeyed host silently gets the keyless
# providers instead of an error. Wire the key with `pix secret set
# PARALLEL_API_KEY op://...` + `pix secret sync`.
COPY --chown=agent:agent web-search.json   /home/agent/.pi/web-search.json
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
# subagents.ts resolves it here. Regenerate on the host with `pix models
# route` after editing services/host/routing/scorecard.json. See
# docs/design/routing.md.
COPY --chown=agent:agent routing.json      /home/agent/.pi/agent/routing.json
COPY --chown=agent:agent skills/       /home/agent/.pi/agent/skills/
COPY --chown=agent:agent extensions/   /home/agent/.pi/agent/extensions/
# lib/ holds code SHARED by extensions but deliberately not loadable as one (pi
# treats every .ts under extensions/ as an extension factory and dies on a file
# that is not). The recall extensions `import … from "../lib/recall-message.ts"`,
# so the directory has to sit next to extensions/ in the image too — leave it out
# and pi fails to load both recall extensions and the agent exits 1 at startup.
COPY --chown=agent:agent lib/          /home/agent/.pi/agent/lib/
COPY --chown=agent:agent agents/       /home/agent/.pi/agent/agents/
COPY --chown=agent:agent themes/       /home/agent/.pi/agent/themes/
# AC-REL-01/02: ship the generated notices + affiliation disclaimer inside the
# image itself (scripts/check-third-party-notices.sh asserts this COPY line
# exists, so the two can't silently drift apart).
COPY --chown=agent:agent THIRD_PARTY_NOTICES.md /home/agent/.pi/agent/THIRD_PARTY_NOTICES.md
COPY --chown=agent:agent NOTICE.md              /home/agent/.pi/agent/NOTICE.md
# The image is a distribution of pix itself (MIT) and of the MPL-2.0 components
# linked into pix-host, so both license texts have to travel WITH it: MIT s2
# ("included in all copies or substantial portions") and MPL-2.0 s3.1/s3.2(b)
# (recipients must be told the terms and how to get a copy of the license).
COPY --chown=agent:agent LICENSE                /home/agent/.pi/agent/LICENSE
COPY --chown=agent:agent licenses/              /home/agent/.pi/agent/licenses/
# Note: company tooling (e.g. a `snow` wrapper) is NOT in the public image. Such
# in-sandbox wrappers are delivered by a pack's `[[proxy]]` bin/ at run time; see
# docs/design/packs.md.

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
      org.opencontainers.image.title="pix" \
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
# EXCEPTION, deliberate: pi-mcp-adapter stays at 2.21.1 across the 0.84.3 bump.
# 2.27.0 (the newest published before pi 0.84.3's release) moves the startup
# anchor apply-mcp-problems-status.mjs matches, so the patch throws
# "startup anchor not found in .../init.ts" and fails the image build. Verified
# by dry-run against a clean 2.27.0 install. Refreshing that patch is its own
# change, not a rider on a pi patch bump.
# EXCEPTION, deliberate: pi-web-access stays at 0.13.0 across the 0.84.1 bump.
# 0.19.0 upstreamed our gateway seams under DIFFERENT config keys
# (openaiResponsesUrl / openaiSearchModel vs. our openaiBaseUrl / openaiModel)
# and dropped the anchors apply-web-access-gateway.mjs matches. That patch is
# fatal-on-mismatch by design, so bumping it fails the image build, and moving
# to the upstream seams renames config keys a private pack reads. Migrate that
# deliberately, in its own change, with the pack in hand.
# NOTE: @tintinweb/pi-subagents stays DISABLED (it hung the event loop forever on
# pi 0.80.x; full trace in docs/upstream/pi-subagents-hang-pi-0.80.md). It is
# REPLACED by our own extensions/subagents.ts, which spawns each child as
# `pi --no-extensions -e <self>` (a fully-loaded child re-binds the ollama/memory
# ports the parent holds and deadlocks at startup — the real freeze) with an
# inactivity + wall-clock watchdog so a stuck subagent is killed, not hung. It is
# a plain baked extension (no npm install line), so nothing to add here; see
# docs/design/subagents-extension.md. Do NOT restore the @tintinweb line.
RUN set -eux; for p in \
      pi-plan@0.1.1 pi-mcp-adapter@2.21.1 \
      pi-manage-todo-list@0.4.0 pi-simplify@0.2.3 pi-web-access@0.13.0 \
      @juanibiapina/pi-extension-settings@0.9.1 \
      pi-usage@0.3.0; do \
      pi install "npm:$p"; \
    done; pi list

# Keep healthy MCP quiet. The adapter's stock footer modes are always-on or
# always-off; this adds a problems-only mode that remains visible after a
# connection drops and preserves the existing per-server failure notifications.
RUN node /usr/local/share/pix/patches/apply-mcp-problems-status.mjs

# pi-manage-todo-list 0.4.0 documents `/todos` as a toggle but only refreshes
# the widget. Add toggle/hide/show controls, Alt+T, persisted visibility, and a
# durable clear marker respected by session resume and compaction continuation.
RUN node /usr/local/share/pix/patches/apply-todo-durable-clear.mjs

# pi-web-access 0.13.0 hardcodes api.openai.com + gpt-5.4 for native Responses
# search. Add the two configuration seams a private pack needs to reuse its
# already trust-gated OpenAI Responses backend. The patch is pinned, exact, and
# fails the image build if the upstream source moves.
RUN node /usr/local/share/pix/patches/apply-web-access-gateway.mjs

# Bound the subagent result-wait so a dead subagent can't park the event loop
# (Esc-proof hang). Idempotent + non-fatal. DISABLED alongside pi-subagents above
# (nothing to patch); it also proved insufficient — on 0.80.x the wait isn't the
# `await` this patches, so it never fired. Re-enable with the extension.
# RUN node /usr/local/share/pix/patches/apply-subagent-timeout.mjs

WORKDIR /home/agent/workspace
CMD ["pi"]
