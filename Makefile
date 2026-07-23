# Set DOCKER_USER to your Docker Hub namespace before `make publish`.
# VERSION is a PINNED tag (never `latest`): Docker re-pulls `:latest` on every
# run even when the image is already loaded, so `make load` would be ignored. A
# pinned tag gets IfNotPresent semantics — use the loaded local build if present,
# else pull once. Keep in sync with `version` in package.json and `image:` in
# pi-kit/spec.yaml.
DOCKER_USER ?= mcavage
VERSION     ?= 0.0.42
# LAUNCHER_VERSION stamps the pi-stack launcher binary. A LOCAL build marks the
# version "+local" so the launcher knows it is UNRELEASED (no matching git tag
# v$(VERSION) exists) and uses the local checkout kit instead of pinning a bogus
# tag. A CI RELEASE build overrides this to a clean X.Y.Z (LAUNCHER_VERSION=$(VERSION)).
LAUNCHER_VERSION ?= $(VERSION)+local
IMAGE       ?= docker.io/$(DOCKER_USER)/pi-stack:$(VERSION)
LATEST      ?= docker.io/$(DOCKER_USER)/pi-stack:latest
KIT         ?= ./pi-kit
# Private overlay — its OWN peer repo (not in this tree). It contributes two halves
# (see docs/OVERLAY.md): kit/ (a mixin kit: company skills, full capabilities.json,
# in-sandbox wrappers — `make run` stacks it) and host/overlay_*.go (host plugins —
# `make serve` symlinks them into services/host/ so they compile into the binary).
# Override OVERLAY on the command line or in the environment (e.g.
# `make run OVERLAY=../my-overlay`) if your peer repo lives elsewhere.
OVERLAY     ?= ../pi-stack-work
OVERLAY_KIT_FLAG = $(if $(wildcard $(OVERLAY)/kit/spec.yaml),--kit $(OVERLAY)/kit,)
# Also mount the overlay's kit/ as a writable extra workspace, so skills/config the
# agent improves mid-session can be saved to the real source (then `make load` bakes
# them). Without this, in-sandbox edits land on the read-only kit-delivered copy and
# die with the sandbox. Scoped to kit/ (not the repo root), so host/*.go and .git
# stay out of the VM. See AGENTS.md "Writing skills".
OVERLAY_WS = $(if $(wildcard $(OVERLAY)/kit/spec.yaml),$(OVERLAY)/kit,)
# Dev mode (Mode B): `make run` launches from the repo, so load skills LIVE from the
# host tree instead of the copies baked into the image — edit a SKILL.md, /reload in
# pi, and it's live, no rebuild. `--no-skills` turns off baked discovery; each
# `--skill <root>` recurses for SKILL.md, so we point at the public repo's skills and
# (if present) the overlay's. Paths are absolute = the in-sandbox mount paths (sbx
# mounts each workspace at its host path). Consumers who `sbx run --kit git+...` never
# hit this target, so they get the baked set (Mode A). See AGENTS.md.
DEV_SKILLS = --no-skills --skill $(CURDIR)/skills$(if $(OVERLAY_WS), --skill $(abspath $(OVERLAY))/kit/files/home/.pi/agent/skills,)
# Runtime config — the SINGLE source of truth is ~/.config/pi-stack/config.toml,
# managed by `pi-stack config set` and read here via `pi-stack config get`
# (profile-aware, list keys space-separated). The `?=` assignments are DEFERRED:
# they only shell out when a target actually expands the value, and a
# command-line/env override (`make run MCP=slack`) still wins. Targets that need
# these values depend on `require-launcher`, which fails loudly if
# $(PI_STACK_BIN) isn't built — never silently runs with empty config.
PI_STACK_BIN ?= $(CURDIR)/out/pi-stack

# MCP enablement for `make run` (config.toml `mcp`, `pi-stack config set mcp
# <name>`). Listed servers are auto-attached (`--mcp <name>`) AND are what
# `make mcp-register` registers among the local stdio servers. EMPTY = dynamic
# mode: the gateway exposes only discovery tools (mcp-find / mcp-exec /
# code-mode) and the agent pulls tools in on demand instead of dumping 100+
# into context. NOTE: local stdio servers (e.g. slack) are NOT surfaced by
# dynamic discovery — to use them they must be in the mcp list so `make run`
# attaches them. `MCP=all` attaches everything registered.
MCP         ?= $(shell "$(PI_STACK_BIN)" config get mcp 2>/dev/null)
MCP_FLAGS   = $(foreach server,$(MCP),--mcp $(server))
# The local stdio MCP servers `make mcp-register` can register (the ones you
# actually use — i.e. those listed in MCP). `slack` is a pi-stack-host subcommand;
# `gog` is the host-side Google Workspace CLI's MCP mode. A private overlay
# (config/overlay.mk) can append more (e.g. bamboohr).
LOCAL_STDIO_MCP = slack gog
REGISTER        = $(filter $(LOCAL_STDIO_MCP),$(MCP))

# Host MCP server credentials all come from 1Password via one file of op:// refs
# (config/op-refs.env), resolved by `op run` when the sbx gateway spawns a server.
# OP_BIN is op's absolute path (the sbx daemon's PATH may not include it).
OP_REFS := $(CURDIR)/config/op-refs.env
OP_BIN  := $(shell command -v op 2>/dev/null)

# Absolute path to the host-side `gog` (Google Workspace) binary. Same rationale
# as OP_BIN: the sbx gateway daemon's PATH may not include it, so we resolve it
# here and register the server with the absolute path. NO literal fallback: if
# `gog` isn't on the make PATH this stays empty and the mcp-register guard trips
# (registering a bare `gog` the gateway daemon can't exec is a silent footgun).
GOG_BIN := $(shell command -v gog 2>/dev/null)

# The Google account the host-side `gog` MCP server runs as (config.toml
# `gog_account`, `pi-stack config set gog_account <email>` — never hardcoded
# here). Passed to `gog --account` when `make mcp-register` registers gog with
# the gateway.
GOG_ACCOUNT ?= $(shell "$(PI_STACK_BIN)" config get gog_account 2>/dev/null)

# The private overlay's make targets (gitignored peer repo) — company-specific
# integrations (Snowflake/BambooHR targets, vars, extra MCP).
-include $(OVERLAY)/overlay.mk

# Local-model deps for the self-learning memory (host Ollama). The watcher model
# turns your messages into durable facts (capture); the embed model powers
# semantic recall. `make pull-models` fetches them. Override with `pi-stack
# config set memory_watcher_model <m>`. The default is small on purpose: it runs
# resident on the HOST during `make serve`, so a big model OOMs a 16GB laptop.
MEMORY_WATCHER_MODEL ?= $(shell "$(PI_STACK_BIN)" config get memory_watcher_model 2>/dev/null)
MEMORY_EMBED_MODEL   ?= $(shell "$(PI_STACK_BIN)" config get memory_embed_model 2>/dev/null)
# OLLAMA_BRIDGE_MODEL: the local model the sandbox's ollama-bridge exposes to pi
# (interactive Alt+P cycle) AND the router's local option. Loads on demand (not
# resident), so it can be bigger than the watcher. `pi-stack run` reads this from
# ~/.config/pi-stack/config.toml (ollama_bridge_model); `make run` writes it into
# the workspace so dev runs pick it up the same way. Keep in sync with the router
# registry id in services/host/routing/defaults/models.json.
OLLAMA_BRIDGE_MODEL ?= $(shell "$(PI_STACK_BIN)" config get ollama_bridge_model 2>/dev/null)

# SERVICES: which host services `make serve` runs (config.toml `services`,
# `pi-stack config set services <name>`). MCP (top of file): which MCP servers
# `make run` auto-attaches and `make mcp-register` registers. config.toml is
# the SINGLE place to configure the stack — you never pass flags by hand.
SERVICES ?= $(shell "$(PI_STACK_BIN)" config get services 2>/dev/null)

# SERVE_ENV: extra `KEY=VALUE` pairs injected into the environment of the host
# services `make serve` starts. The public stack sets nothing here; a private
# overlay (config/overlay.mk) appends the vars its own services need so no
# company-specific env leaks into the public tree. Values are expanded into the
# shell UNQUOTED, so each entry must be a single shell token: no spaces or shell
# metacharacters. For a value that needs them (e.g. a connection string with
# `&` or spaces), have the overlay service read an env FILE instead of passing
# it here. See docs/OVERLAY.md.
# VALIDATION NOTE: SERVE_ENV is passed unquoted and untested — the Makefile
# cannot validate its content. Ensure each entry is a safe shell token before
# setting it (e.g. `SERVE_ENV="KEY=value"` where value has no spaces or &).
SERVE_ENV ?=

# out/ is gitignored, so it's absent on a fresh clone. Several targets (load,
# launcher, serve, mcp-register, pack, …) write into it and would otherwise fail
# with "invalid output path: stat out: no such file or directory". Create it once
# at parse time so every target can rely on it.
$(shell mkdir -p out)

.PHONY: help build load publish validate inspect run run-published run-no-mcp serve doctor memory-serve mcp-register mcp-auth pull-models secrets pack install clean link-overlay launcher route require-launcher doctor-overlay

# Guard for every target that sources runtime config (SERVICES/MCP/GOG_ACCOUNT/
# models) from config.toml: the launcher binary MUST exist, and `config get`
# MUST work — otherwise the $(shell …) sourcing above yields silently-empty
# values. Fail loudly instead.
require-launcher:
	@[ -x "$(PI_STACK_BIN)" ] || { \
		echo "ERROR: $(PI_STACK_BIN) not found. Runtime config (SERVICES/MCP/models) is"; \
		echo "       sourced from ~/.config/pi-stack/config.toml via 'pi-stack config get',"; \
		echo "       so the launcher must be built first: make launcher  (or: make install)"; \
		exit 1; }
	@"$(PI_STACK_BIN)" config get services >/dev/null || { \
		echo "ERROR: '$(PI_STACK_BIN) config get' failed — cannot source runtime config from config.toml."; \
		echo "       Run 'pi-stack config show' to diagnose (bad profile? corrupt config.toml?)."; \
		exit 1; }

# Symlink the private overlay's host plugins ($(OVERLAY)/host/overlay_*.go) into
# services/host/ so they compile into pi-stack-host and self-register. No-op in a
# public clone (the overlay peer repo isn't there) — the binary builds clean.
# The symlinks are gitignored (services/host/overlay_*.go).
link-overlay:
	@rm -f services/host/overlay_*.go
	@if [ -d "$(OVERLAY)/host" ]; then \
		for f in $(OVERLAY)/host/overlay_*.go; do \
			[ -e "$$f" ] && ln -sf "$$(cd "$$(dirname "$$f")" && pwd)/$$(basename "$$f")" services/host/; \
		done; \
	fi

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Runtime, routing, agent, and parallel-task commands live in the launcher,"
	@echo "not make:  pi-stack help --all  (e.g. pi-stack route compile,"
	@echo "pi-stack agent ls, pi-stack task new)."

build: ## Build the pi-stack image from the DHI base
	docker build -t $(IMAGE) .

# CRITICAL: sbx caches a materialized image PER TAG. With a fixed tag (:0.0.1),
# `sbx run` keeps booting the first-cached copy and silently ignores every
# reload — verified by creating sandboxes and finding stale extensions. So we
# tag each build uniquely, load that, and `make run` pins --template to it.
# Old local-*/$(VERSION) templates are pruned so the store doesn't grow.
# (These comments live ABOVE the recipe so make doesn't echo them to the terminal.)
load: build ## Build + load the image into sbx under a UNIQUE tag, so `make run` uses this exact build
	@set -e; TS="local-$$(date +%s)"; T="docker.io/$(DOCKER_USER)/pi-stack:$$TS"; \
	docker tag $(IMAGE) "$$T"; \
	docker save "$$T" -o out/pi-stack.tar; \
	for id in $$(sbx template ls 2>/dev/null | awk '$$1=="docker.io/$(DOCKER_USER)/pi-stack" && ($$2=="$(VERSION)" || $$2 ~ /^local-/){print $$3}'); do sbx template rm "$$id" >/dev/null 2>&1 || true; done; \
	sbx template load out/pi-stack.tar; \
	rm -f out/pi-stack.tar; docker rmi "$$T" >/dev/null 2>&1 || true; \
	echo "$$TS" > out/.local-image-tag; \
	echo "Loaded as :$$TS. To use it: sbx rm -f pi-stack-pi-stack && make run"

publish: build ## Push the built image to the registry as :$(VERSION) and :latest (run `docker login` first)
	docker push $(IMAGE)
	docker tag $(IMAGE) $(LATEST)
	docker push $(LATEST)
	@echo "Published $(IMAGE) and $(LATEST)."
	@echo "  Discoverability tag: $(LATEST) (for manual docker pull / Hub browsing)."
	@echo "  Kit pins :$(VERSION), so consumers + local runs resolve the version (no re-pull)."
	@echo "  Consumers: sbx run pi-stack --kit \"git+https://github.com/$(DOCKER_USER)/pi-stack.git#dir=pi-kit\""

validate: ## Validate the sandbox kit
	sbx kit validate $(KIT)

inspect: ## Inspect the kit
	sbx kit inspect $(KIT)

secrets: ## Store provider keys + GitHub token as global sbx service secrets
	@echo "Store once (read by the host proxy, never stored in the VM):"
	@echo '  echo "$$ANTHROPIC_API_KEY" | sbx secret set -g anthropic'
	@echo '  echo "$$OPENAI_API_KEY"    | sbx secret set -g openai'
	@echo '  echo "$$GEMINI_API_KEY"    | sbx secret set -g google'
	@echo '  gh auth token             | sbx secret set -g github   # gh in-sandbox, no GH_TOKEN export needed'

# NOTE: NAME must not contain spaces or shell metacharacters — the awk -v
# assignment below is not quoted against them. The default naming convention
# (pi-stack-<dir>) is safe. Non-default names must follow the same rule.
NAME ?= pi-stack-pi-stack
run: require-launcher ## Launch a pi-stack sandbox NAME. If NAME is stopped it's recreated (workspace + .pi-sessions are host-mounted, so nothing is lost); if it's already running this refuses rather than clobber a live session. `make run NAME=pi-stack-2` opens a second parallel sandbox in another window. (Kit-defined agents can't be re-attached, hence recreate.)
	@status=$$(sbx ls 2>/dev/null | awk -v n="$(NAME)" '$$1==n{print $$3}'); \
	if [ "$$status" = "running" ]; then \
		echo "ERROR: sandbox $(NAME) is already running (a live pi). Use a different name (make run NAME=pi-stack-2) or 'sbx rm -f $(NAME)' first."; exit 1; \
	fi; \
	if [ -n "$$status" ]; then \
		echo "(sandbox $(NAME) exists [$$status] — recreating; workspace + .pi-sessions persist on the host)"; \
		sbx rm -f $(NAME) >/dev/null 2>&1 || true; \
	fi; \
	TAG=$$(cat out/.local-image-tag 2>/dev/null || true); \
	[ -n "$$TAG" ] && echo "(new sandbox $(NAME), local build :$$TAG)" || echo "(new sandbox $(NAME), kit-pinned image)"; \
	mkdir -p .pi-stack && echo "$(OLLAMA_BRIDGE_MODEL)" > .pi-stack/ollama-bridge.model; \
	exec sbx run pi-stack --name $(NAME) $${TAG:+--template docker.io/$(DOCKER_USER)/pi-stack:$$TAG} --kit $(KIT) $(OVERLAY_KIT_FLAG) $(MCP_FLAGS) . $(OVERLAY_WS) -- $(DEV_SKILLS)

# Run the latest PUBLISHED image straight off the git-hosted kit — the true
# consumer path, no local repo needed. Every push to main auto-publishes a NEW
# version (CI stamps 0.0.<run_number>, see .github/workflows/publish.yml) and
# commits the bump into pi-kit/spec.yaml on main. Since the kit reads spec.yaml
# from main, this always pins a version sbx has NEVER cached → a fresh pull with
# no --template override and no `sbx template rm` dance. Wait for CI green first.
run-published: ## Run the latest PUBLISHED image via the git kit (always fresh — every push is a new version tag; no eviction needed). `make run` = local build.
	-sbx rm -f pi-stack-published >/dev/null 2>&1
	@sbx run pi-stack --name pi-stack-published --kit "git+https://github.com/$(DOCKER_USER)/pi-stack.git#dir=pi-kit"

run-no-mcp: ## Launch without sbx Cloud MCP Gateway, for debugging MCP setup failures
	@env -u SBX_MCP_URL sbx run pi-stack --kit $(KIT) .

launcher: ## Build BOTH host binaries (out/pi-stack launcher + out/pi-stack-host services), version-stamped (local builds stamp $(VERSION)+local so the launcher uses the local kit, not a nonexistent v$(VERSION) tag)
	(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pi-stack ./cmd/pi-stack)
	(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .)
	@echo "Built out/pi-stack + out/pi-stack-host (version $(LAUNCHER_VERSION))."
	@echo "Install both: ln -sf $(CURDIR)/out/pi-stack ~/.local/bin/pi-stack && ln -sf $(CURDIR)/out/pi-stack-host ~/.local/bin/pi-stack-host"

memory-serve: link-overlay ## Build + run just the memory service (JSON-RPC :11435) from pi-stack-host
	(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) && exec ./out/pi-stack-host memory

mcp-auth: ## (Re)authorize the remote OAuth MCP servers (opine/granola/notion/atlassian). Run this when standup/refresh reports them "not in the gateway" — sbx's hosted MCP OAuth creds do NOT persist reliably across sessions/daemon restarts (they silently drop to "Not Found"), so re-establishing them is a recurring chore until sbx fixes it. Opens a browser per server.
	@command -v sbx >/dev/null 2>&1 || { echo "ERROR: sbx not found"; exit 1; }
	@echo "1/3 refreshing the control-plane session (sbx login)…"
	sbx login
	@echo "2/3 authorizing all registered remote OAuth servers…"
	sbx mcp auth --all
	@echo "3/3 status:"
	-sbx mcp auth status --all
	@echo ""
	@echo "If all show authorized, recreate to pick them up: sbx rm -f pi-stack-pi-stack && make run"

mcp-register: require-launcher link-overlay ## Register the local stdio MCP servers you use (the mcp list in config.toml) with sbx. The gateway runs each as `op run --env-file=config/op-refs.env -- pi-stack-host <name>`, so creds come from 1Password at spawn (nothing stored in the registration). Needs SBX_MCP_URL + op + config/op-refs.env.
	@command -v sbx >/dev/null 2>&1 || { echo "ERROR: sbx not found"; exit 1; }
	@[ -n "$(strip $(REGISTER))" ] || { echo "Nothing to register: no local stdio servers ($(LOCAL_STDIO_MCP)) are in MCP. Run: pi-stack config set mcp <name>."; exit 0; }
	@[ -n "$$SBX_MCP_URL" ] || { echo "ERROR: SBX_MCP_URL is unset — MCP is not enabled, so 'sbx mcp add' will fail."; \
		echo "  Fix (once):  export SBX_MCP_URL=https://gateway.docker.com  &&  sbx daemon stop"; exit 1; }
	@[ -n "$(OP_BIN)" ] || { echo "ERROR: 1Password CLI 'op' not found on PATH."; exit 1; }
	@[ -f "$(OP_REFS)" ] || { echo "ERROR: $(OP_REFS) missing. Create it:  cp config/op-refs.env.example config/op-refs.env  then fill in your refs."; exit 1; }
	@[ -z "$(filter gog,$(REGISTER))" ] || [ -n "$(GOG_ACCOUNT)" ] || { echo "ERROR: gog is in MCP but gog_account is unset. Run: pi-stack config set gog_account <you@example.com>."; exit 1; }
	@[ -z "$(filter gog,$(REGISTER))" ] || [ -n "$(GOG_BIN)" ] || { echo 'ERROR: gog not found on PATH — brew install gog'; exit 1; }
	@(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .)
	@BIN="$(CURDIR)/out/pi-stack-host"; \
	for s in $(REGISTER); do \
		case "$$s" in \
		gog) \
			: 'HARDENED registration (security-lead): read-only by default —'; \
			: '--gmail-no-send + --wrap-untrusted + --readonly + `mcp --allow-tool read`.'; \
			: 'Creds resolve from config/op-refs.env via op run at gateway spawn.'; \
			: 'Absolute op path ($(OP_BIN)) + resolved gog ($(GOG_BIN)) so the gateway daemon PATH need not include them.'; \
			sbx mcp add gog --command "$(OP_BIN)" --args run --args --no-masking --args "--env-file=$(OP_REFS)" --args -- --args "$(GOG_BIN)" --args --account --args $(GOG_ACCOUNT) --args --gmail-no-send --args --wrap-untrusted --args --readonly --args mcp --args --allow-tool --args read \
				&& echo "  registered: gog" || echo "  FAILED to register: gog" ;; \
		*) \
			sbx mcp add $$s --command "$(OP_BIN)" \
				--args run --args --no-masking --args "--env-file=$(OP_REFS)" --args -- --args "$$BIN" --args mcp --args "$$s" \
				&& echo "  registered: $$s" || echo "  FAILED to register: $$s" ;; \
		esac; \
	done
	@echo "Verify: sbx mcp ls"
	@echo "Attach: registration is NOT enough — a sandbox only gets these if you START it with them."
	@echo "        \`make run\` does this for you (MCP=$(MCP) from config.toml). Local stdio"
	@echo "        servers aren't surfaced by dynamic discovery, and this sbx can't attach to a"
	@echo "        running sandbox — so just \`make run\` (it passes --mcp for each)."
	@echo "        Local stdio servers are NOT surfaced by dynamic mcp-find, and this sbx has no 'mcp load'"
	@echo "        for a running sandbox — so re-run with --mcp to pick them up."
	@echo "Note: each server resolves its creds from config/op-refs.env via op run when the gateway spawns it — make sure those refs are filled + valid."

serve: require-launcher link-overlay ## Start the host services named in SERVICES (config.toml `services`): memory :11435. MCP servers (slack, gog) are run by the sbx gateway — see `make mcp-register`. Ctrl-C stops all.
	@echo "Host services [$(SERVICES)] — sandboxes reach these on host.docker.internal. Ctrl-C stops all."
	@(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) || { echo "go build failed (pi-stack-host)"; exit 1; }
	@exec env $(SERVE_ENV) MEMORY_WATCHER_MODEL=$(MEMORY_WATCHER_MODEL) MEMORY_EMBED_MODEL=$(MEMORY_EMBED_MODEL) out/pi-stack-host serve $(SERVICES)

# route is MAINTAINER tooling for the model router, run from the repo (it reads
# services/host/routing/). It is NOT part of the consumer surface: `route` is
# deliberately NOT a `pi-stack` command — it lives here in the Makefile, invoking
# the repo-built pi-stack-host backend. See the `model-refresh` skill +
# docs/design/routing.md. Scores are hand-maintained in
# services/host/routing/defaults/scorecard.json — edit it, then `make route
# ARGS=compile` (or `pi-stack route compile`).
# Bare `make route` defaults to the safe, read-only `show` (the scorecard /
# resolved table) so it never errors without ARGS.
route: ## Model router (maintainer): make route ARGS="show" | "models" | "compile" | "pick <intent>"
	@(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) && ./out/pi-stack-host route $(if $(strip $(ARGS)),$(ARGS),show)

pull-models: require-launcher ## Pull the local Ollama models the stack uses (memory watcher + embed, and the bridge/router local model)
	@command -v ollama >/dev/null 2>&1 || { echo "ollama not installed — see https://ollama.com (optional: enables semantic recall + fact capture + the local model)"; exit 1; }
	@echo "Pulling watcher model: $(MEMORY_WATCHER_MODEL)"; ollama pull $(MEMORY_WATCHER_MODEL)
	@echo "Pulling embed model:   $(MEMORY_EMBED_MODEL)";   ollama pull $(MEMORY_EMBED_MODEL)
	@echo "Pulling local model:   $(OLLAMA_BRIDGE_MODEL)";   ollama pull $(OLLAMA_BRIDGE_MODEL)
	@echo "Done. 'make doctor' will now show capture + semantic recall as ready."

doctor: require-launcher ## Show models + each optional integration: set up? service running?
	@port() { nc -z localhost "$$1" >/dev/null 2>&1 && echo "up" || echo "down"; }; \
	sset() { sbx secret ls 2>/dev/null | grep -qw "$$1" && echo "sbx secret set" || echo "TODO: sbx secret set -g $$1"; }; \
	model() { command -v ollama >/dev/null 2>&1 && ollama list 2>/dev/null | grep -q "^$$1\b" && echo "pulled" || echo "TODO: ollama pull $$1 (or make pull-models)"; }; \
	echo "Config (config.toml via 'pi-stack config get' — the single source of truth):"; \
	printf "  %-9s %s\n" "SERVICES" "$(SERVICES)   (make serve runs these)"; \
	printf "  %-9s %s\n" "MCP"      "$(if $(strip $(MCP)),$(MCP),<empty: dynamic discovery only>)   (make run attaches these)"; \
	echo ""; \
	echo "Models / providers (proxy-injected, never in the VM):"; \
	printf "  %-9s %s\n" "anthropic" "$$(sset anthropic)"; \
	printf "  %-9s %s\n" "openai"    "$$(sset openai)"; \
	printf "  %-9s %s\n" "google"    "$$(sset google)"; \
	printf "  %-9s %s\n" "ollama"    "$$(command -v ollama >/dev/null 2>&1 && echo installed, :11434 $$(port 11434) || echo 'not installed (optional, for local models)')"; \
	printf "  %-9s %s\n" "  watcher" "$$(model $(MEMORY_WATCHER_MODEL)) — fact capture [$(MEMORY_WATCHER_MODEL)]"; \
	printf "  %-9s %s\n" "  embed"   "$$(model $(MEMORY_EMBED_MODEL)) — semantic recall [$(MEMORY_EMBED_MODEL)]"; \
	echo ""; \
	echo "Data tools (host side):"; \
	printf "  %-7s setup: %-30s serving: %s\n" "gh"    "$$(sset github)" "proxy-injected (no service)"; \
	printf "  %-7s setup: %-30s serving: %s\n" "gog"   "$$(command -v gog >/dev/null 2>&1 && echo 'CLI installed' || echo 'TODO: brew install gog + gog auth')" "MCP via gateway (register: make mcp-register)"; \
	printf "  %-7s setup: %-30s serving: %s\n" "memory" "watcher+embed above" ":11435 $$(port 11435) (capture needs the watcher model)"; \
	echo ""; \
	echo "MCP servers (local stdio, run by the sbx gateway — register with 'make mcp-register', attach with 'make run'):"; \
	reg() { sbx mcp ls 2>/dev/null | grep -qw "$$1" && echo "registered" || echo "TODO: make mcp-register"; }; \
	printf "  %-7s %-14s %s\n" "slack"  "$$(reg slack)"    "$(if $(filter slack,$(MCP)),auto-attached on make run,NOT in MCP — 'pi-stack config set mcp slack' to use)"; \
	printf "  %-7s %-14s %s\n" "gog"    "$$(reg gog)"      "$(if $(filter gog,$(MCP)),auto-attached on make run,NOT in MCP — 'pi-stack config set mcp gog' to use)"; \
	echo "  gateway catalog (atlassian/notion/granola/linear/...): sbx mcp add … then pi-stack config set mcp <name>"; \
	echo ""; \
	echo "All of the above is configured in ~/.config/pi-stack/config.toml (pi-stack config set). Start it: make serve (host) + make run (sandbox)."
	@$(MAKE) -s doctor-overlay 2>/dev/null || true

pack: ## Package the kit as a distributable zip
	sbx kit pack $(KIT) -o out/pi-stack-kit.zip

install: launcher ## Build + put the Go binaries (out/pi-stack launcher + out/pi-stack-host) on your PATH (~/.local/bin)
	mkdir -p $(HOME)/.local/bin
	ln -sf $(CURDIR)/out/pi-stack $(HOME)/.local/bin/pi-stack
	ln -sf $(CURDIR)/out/pi-stack-host $(HOME)/.local/bin/pi-stack-host
	@echo "Installed: pi-stack -> $(CURDIR)/out/pi-stack"
	@echo "Installed: pi-stack-host -> $(CURDIR)/out/pi-stack-host"
	@# Drop the man page on the user manpath too (bonus; the binary embed is the
	@# guarantee, so `pi-stack man` works with or without this). No sudo.
	mkdir -p $(HOME)/.local/share/man/man1
	cp services/host/cmd/pi-stack/pi-stack.1 $(HOME)/.local/share/man/man1/pi-stack.1
	@echo "Installed: man page -> $(HOME)/.local/share/man/man1/pi-stack.1"
	@manpath 2>/dev/null | tr ':' '\n' | grep -qx "$(HOME)/.local/share/man" \
		|| echo "Tip: add ~/.local/share/man to MANPATH for \`man pi-stack\` (or just use \`pi-stack man\`)."
	@echo "Runtime config lives in ~/.config/pi-stack/config.toml — manage it with"
	@echo "'pi-stack config set <key> <value>' (or 'pi-stack setup' for the guided flow)."
	@echo "Ensure ~/.local/bin is on your PATH, then: cd <any project> && pi-stack"

clean: ## Remove the built image
	-docker rmi $(IMAGE)
