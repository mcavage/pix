# Set DOCKER_USER to your Docker Hub namespace before `make publish`.
# VERSION is a PINNED tag (never `latest`): Docker re-pulls `:latest` on every
# run even when the image is already loaded, so `make load` would be ignored. A
# pinned tag gets IfNotPresent semantics — use the loaded local build if present,
# else pull once. Keep in sync with `version` in package.json and `image:` in
# pi-kit/spec.yaml.
DOCKER_USER ?= mcavage
VERSION     ?= 0.0.16
IMAGE       ?= docker.io/$(DOCKER_USER)/pi-stack:$(VERSION)
LATEST      ?= docker.io/$(DOCKER_USER)/pi-stack:latest
KIT         ?= ./pi-kit
# Private overlay — its OWN peer repo (not in this tree). It contributes two halves
# (see docs/OVERLAY.md): kit/ (a mixin kit: company skills, full capabilities.json,
# in-sandbox wrappers — `make run` stacks it) and host/overlay_*.go (host plugins —
# `make serve` symlinks them into services/host/ so they compile into the binary).
# Set OVERLAY in config/local.mk if your peer repo lives elsewhere.
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
# MCP enablement for `make run`. Set this in config/local.mk (written by
# `make install`) so the stack is configured once, not by hand each run. Listed
# servers are auto-attached (`--mcp <name>`) AND are what `make mcp-register`
# registers among the local stdio servers. EMPTY = dynamic mode: the gateway
# exposes only discovery tools (mcp-find / mcp-exec / code-mode) and the agent
# pulls tools in on demand instead of dumping 100+ into context. NOTE: local
# stdio servers (e.g. slack) are NOT surfaced by dynamic discovery — to use
# them they must be listed here so `make run` attaches them. `MCP=all` attaches
# everything registered.
MCP         ?=
MCP_FLAGS   = $(foreach server,$(MCP),--mcp $(server))
# The local stdio MCP servers this host binary implements. `make mcp-register`
# registers the ones you actually use — i.e. those listed in MCP. A private overlay
# (config/overlay.mk) can append more (e.g. bamboohr).
LOCAL_STDIO_MCP = slack
REGISTER        = $(filter $(LOCAL_STDIO_MCP),$(MCP))

# Host MCP server credentials all come from 1Password via one file of op:// refs
# (config/op-refs.env), resolved by `op run` when the sbx gateway spawns a server.
# OP_BIN is op's absolute path (the sbx daemon's PATH may not include it).
OP_REFS := $(CURDIR)/config/op-refs.env
OP_BIN  := $(shell command -v op 2>/dev/null)

# Owner-specific values live in a gitignored local override so the committed
# defaults stay generic. config/overlay.mk (also gitignored) adds private,
# company-specific integrations (Snowflake/BambooHR targets, vars, extra MCP).
-include config/local.mk
-include $(OVERLAY)/overlay.mk

# Local-model deps for the self-learning memory (host Ollama). The watcher model
# turns your messages into durable facts (capture); the embed model powers
# semantic recall. `make pull-models` fetches them. Override MEMORY_WATCHER_MODEL
# in config/local.mk to use a different one.
MEMORY_WATCHER_MODEL ?= gemma4
MEMORY_EMBED_MODEL   ?= nomic-embed-text

# SERVICES: which host services `make serve` runs (memory, gws). MCP (top of
# file): which MCP servers `make run` auto-attaches and `make mcp-register`
# registers. Both are the SINGLE place to configure the stack — set them in
# config/local.mk (written by `make install`) so you never pass flags by hand.
SERVICES ?= memory gws

.PHONY: help build load publish validate inspect run run-published run-no-mcp serve doctor memory-serve gws-token-serve mcp-register mcp-auth pull-models secrets pack install clean link-overlay launcher

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
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the pi-stack image from the DHI base
	docker build -t $(IMAGE) .

load: build ## Build + load the image into sbx under a UNIQUE tag, so `make run` uses this exact build
	# CRITICAL: sbx caches a materialized image PER TAG. With a fixed tag (:0.0.1),
	# `sbx run` keeps booting the first-cached copy and silently ignores every
	# reload — verified by creating sandboxes and finding stale extensions. So we
	# tag each build uniquely, load that, and `make run` pins --template to it.
	# Old local-*/$(VERSION) templates are pruned so the store doesn't grow.
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

NAME ?= pi-stack-pi-stack
run: ## Launch a pi-stack sandbox NAME. If NAME is stopped it's recreated (workspace + .pi-sessions are host-mounted, so nothing is lost); if it's already running this refuses rather than clobber a live session. `make run NAME=pi-stack-2` opens a second parallel sandbox in another window. (Kit-defined agents can't be re-attached, hence recreate.)
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
	: 'Broker bearer: read-or-mint the shared token the in-sandbox clients (gws'; \
	: 'wrapper, recall) use to authenticate to the host services. Same file the'; \
	: 'launcher (out/pi-stack) and pi-stack-host read/write, so whoever creates'; \
	: 'it first wins and everyone agrees on the value.'; \
	: 'SECURITY: the token is EXPORTED into the sbx PROCESS ENV, not passed as a'; \
	: '--env argv flag. argv is process-inspectable (ps/EDR); the process env is'; \
	: 'not. pi-kit/spec.yaml declares GWS_TOKEN_AUTH so the VM picks it up from'; \
	: 'this forwarded host env. Mirrors cmd/pi-stack/run.go.'; \
	: 'TODO(host-verified): confirm sbx forwards this process-env var into the VM'; \
	: 'per pi-kit/spec.yaml; if not, switch to `sbx secret set` on the host.'; \
	TOKFILE="$${XDG_CONFIG_HOME:-$$HOME/.config}/pi-stack/broker-token"; \
	if [ ! -f "$$TOKFILE" ]; then mkdir -p "$$(dirname "$$TOKFILE")"; (umask 077; head -c32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=' > "$$TOKFILE"); fi; \
	export GWS_TOKEN_AUTH="$$(cat "$$TOKFILE")"; \
	exec sbx run pi-stack --name $(NAME) $${TAG:+--template docker.io/$(DOCKER_USER)/pi-stack:$$TAG} --kit $(KIT) $(OVERLAY_KIT_FLAG) $(MCP_FLAGS) . $(OVERLAY_WS) -- $(DEV_SKILLS)

# Run the latest PUBLISHED image straight off the git-hosted kit — the true
# consumer path, no local repo needed. Every push to main auto-publishes a NEW
# version (CI stamps 0.0.<run_number>, see .github/workflows/publish.yml) and
# commits the bump into pi-kit/spec.yaml on main. Since the kit reads spec.yaml
# from main, this always pins a version sbx has NEVER cached → a fresh pull with
# no --template override and no `sbx template rm` dance. Wait for CI green first.
run-published: ## Run the latest PUBLISHED image via the git kit (always fresh — every push is a new version tag; no eviction needed). `make run` = local build.
	-sbx rm -f pi-stack-published >/dev/null 2>&1
	sbx run pi-stack --name pi-stack-published --kit "git+https://github.com/$(DOCKER_USER)/pi-stack.git#dir=pi-kit"

run-no-mcp: ## Launch without sbx Cloud MCP Gateway, for debugging MCP setup failures
	env -u SBX_MCP_URL sbx run pi-stack --kit $(KIT) .

launcher: ## Build the standalone pi-stack launcher binary (out/pi-stack), version-stamped from VERSION
	(cd services/host && go build -ldflags "-X main.version=$(VERSION)" -o $(CURDIR)/out/pi-stack ./cmd/pi-stack)
	@echo "Built out/pi-stack (version $(VERSION)). Install it with: ln -sf $(CURDIR)/out/pi-stack ~/.local/bin/pi-stack"

memory-serve: link-overlay ## Build + run just the memory service (JSON-RPC :11435) from pi-stack-host
	(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) && exec ./out/pi-stack-host memory

gws-token-serve: link-overlay ## Build + run just the gws bearer token service (:11441) from pi-stack-host
	(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) && exec ./out/pi-stack-host gws-token

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

mcp-register: link-overlay ## Register the local stdio MCP servers you use (the ones in MCP, config/local.mk) with sbx. The gateway runs each as `op run --env-file=config/op-refs.env -- pi-stack-host <name>`, so creds come from 1Password at spawn (nothing stored in the registration). Needs SBX_MCP_URL + op + config/op-refs.env.
	@command -v sbx >/dev/null 2>&1 || { echo "ERROR: sbx not found"; exit 1; }
	@[ -n "$(strip $(REGISTER))" ] || { echo "Nothing to register: no local stdio servers ($(LOCAL_STDIO_MCP)) are in MCP. Set MCP in config/local.mk."; exit 0; }
	@[ -n "$$SBX_MCP_URL" ] || { echo "ERROR: SBX_MCP_URL is unset — MCP is not enabled, so 'sbx mcp add' will fail."; \
		echo "  Fix (once):  export SBX_MCP_URL=https://gateway.docker.com  &&  sbx daemon stop"; exit 1; }
	@[ -n "$(OP_BIN)" ] || { echo "ERROR: 1Password CLI 'op' not found on PATH."; exit 1; }
	@[ -f "$(OP_REFS)" ] || { echo "ERROR: $(OP_REFS) missing. Create it:  cp config/op-refs.env.example config/op-refs.env  then fill in your refs."; exit 1; }
	@(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .)
	@BIN="$(CURDIR)/out/pi-stack-host"; \
	for s in $(REGISTER); do \
		sbx mcp add $$s --command "$(OP_BIN)" \
			--args run --args --no-masking --args "--env-file=$(OP_REFS)" --args -- --args "$$BIN" --args mcp --args "$$s" \
			&& echo "  registered: $$s" || echo "  FAILED to register: $$s"; \
	done
	@echo "Verify: sbx mcp ls"
	@echo "Attach: registration is NOT enough — a sandbox only gets these if you START it with them."
	@echo "        \`make run\` does this for you (MCP=$(MCP) from config/local.mk). Local stdio"
	@echo "        servers aren't surfaced by dynamic discovery, and this sbx can't attach to a"
	@echo "        running sandbox — so just \`make run\` (it passes --mcp for each)."
	@echo "        Local stdio servers are NOT surfaced by dynamic mcp-find, and this sbx has no 'mcp load'"
	@echo "        for a running sandbox — so re-run with --mcp to pick them up."
	@echo "Note: each server resolves its creds from config/op-refs.env via op run when the gateway spawns it — make sure those refs are filled + valid."

serve: link-overlay ## Start the host services named in SERVICES (config/local.mk): memory :11435, gws :11441. MCP servers (slack) are run by the sbx gateway — see `make mcp-register`. Ctrl-C stops all.
	@echo "Host services [$(SERVICES)] — sandboxes reach these on host.docker.internal. Ctrl-C stops all."
	@(cd services/host && go build -o $(CURDIR)/out/pi-stack-host .) || { echo "go build failed (pi-stack-host)"; exit 1; }
	@exec env SNOW_CONN=$(SNOW_CONN) MEMORY_WATCHER_MODEL=$(MEMORY_WATCHER_MODEL) MEMORY_EMBED_MODEL=$(MEMORY_EMBED_MODEL) out/pi-stack-host serve $(SERVICES)

pull-models: ## Pull the local Ollama models the memory loop needs (watcher + embed)
	@command -v ollama >/dev/null 2>&1 || { echo "ollama not installed — see https://ollama.com (optional: enables semantic recall + fact capture)"; exit 1; }
	@echo "Pulling watcher model: $(MEMORY_WATCHER_MODEL)"; ollama pull $(MEMORY_WATCHER_MODEL)
	@echo "Pulling embed model:   $(MEMORY_EMBED_MODEL)";   ollama pull $(MEMORY_EMBED_MODEL)
	@echo "Done. 'make doctor' will now show capture + semantic recall as ready."

doctor: ## Show models + each optional integration: set up? service running?
	@port() { nc -z localhost "$$1" >/dev/null 2>&1 && echo "up" || echo "down"; }; \
	sset() { sbx secret ls 2>/dev/null | grep -qw "$$1" && echo "sbx secret set" || echo "TODO: sbx secret set -g $$1"; }; \
	model() { command -v ollama >/dev/null 2>&1 && ollama list 2>/dev/null | grep -q "^$$1\b" && echo "pulled" || echo "TODO: ollama pull $$1 (or make pull-models)"; }; \
	echo "Config (config/local.mk — the single source of truth):"; \
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
	printf "  %-7s setup: %-30s serving: %s\n" "gws"   "$$(command -v gws >/dev/null 2>&1 && echo 'CLI installed' || echo 'TODO: install gws + auth')" ":11441 $$(port 11441)"; \
	printf "  %-7s setup: %-30s serving: %s\n" "memory" "watcher+embed above" ":11435 $$(port 11435) (capture needs the watcher model)"; \
	echo ""; \
	echo "MCP servers (local stdio, run by the sbx gateway — register with 'make mcp-register', attach with 'make run'):"; \
	reg() { sbx mcp ls 2>/dev/null | grep -qw "$$1" && echo "registered" || echo "TODO: make mcp-register"; }; \
	printf "  %-7s %-14s %s\n" "slack"  "$$(reg slack)"    "$(if $(filter slack,$(MCP)),auto-attached on make run,NOT in MCP — add to config/local.mk to use)"; \
	echo "  gateway catalog (atlassian/notion/granola/linear/...): sbx mcp add … then add to MCP in config/local.mk"; \
	echo ""; \
	echo "All of the above is configured in config/local.mk. Start it: make serve (host) + make run (sandbox)."
	@$(MAKE) -s doctor-overlay 2>/dev/null || true

pack: ## Package the kit as a distributable zip
	sbx kit pack $(KIT) -o out/pi-stack-kit.zip

install: ## Put the `pi-stack` launcher on your PATH (~/.local/bin) + create config/local.mk (your stack config) if missing
	mkdir -p $(HOME)/.local/bin
	ln -sf $(CURDIR)/bin/pi-stack $(HOME)/.local/bin/pi-stack
	@echo "Installed: pi-stack -> $(CURDIR)/bin/pi-stack"
	@if [ ! -f config/local.mk ]; then \
		cp config/local.mk.example config/local.mk; \
		echo "Created config/local.mk — edit it to pick SERVICES, MCP, models, then: make serve / make run"; \
	else \
		echo "config/local.mk already present — left as-is (compare with config/local.mk.example for new options)."; \
	fi
	@echo "Ensure ~/.local/bin is on your PATH, then: cd <any project> && pi-stack"

clean: ## Remove the built image
	-docker rmi $(IMAGE)
