# Set DOCKER_USER to your Docker Hub namespace before `make publish`.
# VERSION is a PINNED tag (never `latest`): Docker re-pulls `:latest` on every
# run even when the image is already loaded, so `make load` would be ignored. A
# pinned tag gets IfNotPresent semantics — use the loaded local build if present,
# else pull once. Keep in sync with `version` in package.json and `image:` in
# pi-kit/spec.yaml.
DOCKER_USER ?= mcavage
VERSION     ?= 0.1.7
# LAUNCHER_VERSION stamps the pix launcher binary. A LOCAL build marks the
# version "+local" so the launcher knows it is UNRELEASED (no matching git tag
# v$(VERSION) exists) and uses the local checkout kit instead of pinning a bogus
# tag. A CI RELEASE build overrides this to a clean X.Y.Z (LAUNCHER_VERSION=$(VERSION)).
LAUNCHER_VERSION ?= $(VERSION)+local
IMAGE       ?= docker.io/$(DOCKER_USER)/pix:$(VERSION)
LATEST      ?= docker.io/$(DOCKER_USER)/pix:latest
KIT         ?= ./pi-kit
# Dev mode (Mode B): `make run` launches from the repo, so load skills LIVE from the
# host tree instead of the copies baked into the image — edit a SKILL.md, /reload in
# pi, and it's live, no rebuild. `--no-skills` turns off baked discovery; `--skill
# <root>` recurses for SKILL.md. Company/private context is NOT compiled into
# the image — it's a pack (`pix pack use <path>`, used with the installed binary), and
# host-executing integrations ship as containers the sbx gateway runs (see the
# pix-docker-integrations repo). Consumers who `sbx run --kit git+...` never hit this
# target, so they get the baked set (Mode A). See AGENTS.md.
DEV_SKILLS = --no-skills --skill $(CURDIR)/skills
# Runtime config — the SINGLE source of truth is ~/.config/pix/config.toml,
# managed by `pix config set` and read here via `pix config get`
# (profile-aware, list keys space-separated). The `?=` assignments are DEFERRED:
# they only shell out when a target actually expands the value, and a
# command-line/env override (`make run MCP=slack`) still wins. Targets that need
# these values depend on `require-launcher`, which fails loudly if
# $(PIX_BIN) isn't built — never silently runs with empty config.
PIX_BIN ?= $(CURDIR)/out/pix

# MCP enablement for `make run` (config.toml `mcp`, `pix config set mcp
# <name>`). NOTE: `make run` execs `sbx run` DIRECTLY, so MCP_FLAGS attaches every
# listed server STATICALLY (`--static-mcp <name>`) at sandbox CREATE — the dev
# flow gets all tools. Every configured server preloads this way, with no
# eager/lazy or static/dynamic split (the retired `mcp_static`/`mcp_dynamic`
# knobs are gone): the `pix run` launcher resolves the exact same set via
# allPreloadedMCP, which is what consumers use. sbx's local data-plane gateway
# serves them (no SBX_MCP_URL). Attach one to an ALREADY-RUNNING sandbox with
# `pix mcp load <name>`. `MCP=all` = everything registered.
MCP         ?= $(shell "$(PIX_BIN)" config get mcp 2>/dev/null)
MCP_FLAGS   = $(foreach server,$(MCP),--static-mcp $(server))
# The local stdio MCP servers `make mcp-register` can register (the ones you
# actually use — i.e. those listed in MCP). `slack` is a pix-host subcommand;
# `google-workspace` is the host-side Google Workspace CLI's MCP mode. Additional integrations
# are packs: remote catalog servers, or containers the gateway runs.
LOCAL_STDIO_MCP = slack google-workspace
REGISTER        = $(filter $(LOCAL_STDIO_MCP),$(MCP))

# Host MCP server credentials all come from 1Password via one file of op:// refs
# (config/op-refs.env), resolved by `op run` when the sbx gateway spawns a server.
# OP_BIN is op's absolute path (the sbx daemon's PATH may not include it).
OP_REFS := $(CURDIR)/config/op-refs.env
OP_BIN  := $(shell command -v op 2>/dev/null)


# Local-model deps for the self-learning memory (host Ollama). The watcher model
# turns your messages into durable facts (capture); the embed model powers
# semantic recall. `make pull-models` fetches them. Override with `pix
# config set memory_watcher_model <m>`. The default is small on purpose: it runs
# resident on the HOST during `make serve`, so a big model OOMs a 16GB laptop.
MEMORY_WATCHER_MODEL ?= $(shell "$(PIX_BIN)" config get memory_watcher_model 2>/dev/null)
MEMORY_EMBED_MODEL   ?= $(shell "$(PIX_BIN)" config get memory_embed_model 2>/dev/null)
# OLLAMA_BRIDGE_MODEL: the local model the sandbox's ollama-bridge exposes to pi
# (interactive Alt+P cycle) AND the router's local option. Loads on demand (not
# resident), so it can be bigger than the watcher. `pix run` reads this from
# ~/.config/pix/config.toml (ollama_bridge_model); `make run` writes it into
# the workspace so dev runs pick it up the same way. Keep in sync with the router
# registry id in services/host/routing/defaults/models.json.
OLLAMA_BRIDGE_MODEL ?= $(shell "$(PIX_BIN)" config get ollama_bridge_model 2>/dev/null)

# SERVICES: which host services `make serve` runs (config.toml `services`,
# `pix config set services <name>`). MCP (top of file): which MCP servers
# `make run` auto-attaches and `make mcp-register` registers. config.toml is
# the SINGLE place to configure the stack — you never pass flags by hand.
SERVICES ?= $(shell "$(PIX_BIN)" config get services 2>/dev/null)

# SERVE_ENV: extra `KEY=VALUE` pairs injected into the environment of the host
# services `make serve` starts. The stack sets nothing here by default. Values are
# expanded into the shell UNQUOTED, so each entry must be a single shell token: no
# spaces or shell metacharacters. For a value that needs them (e.g. a connection
# string with `&` or spaces), have the service read an env FILE instead of passing
# it here.
# VALIDATION NOTE: SERVE_ENV is passed unquoted and untested — the Makefile
# cannot validate its content. Ensure each entry is a safe shell token before
# setting it (e.g. `SERVE_ENV="KEY=value"` where value has no spaces or &).
SERVE_ENV ?=

# out/ is gitignored, so it's absent on a fresh clone. Several targets (load,
# launcher, serve, mcp-register, pack, …) write into it and would otherwise fail
# with "invalid output path: stat out: no such file or directory". Create it once
# at parse time so every target can rely on it.
$(shell mkdir -p out)

.PHONY: help build load publish validate inspect run run-published run-no-mcp serve doctor memory-serve mcp-register mcp-auth pull-models secrets pack install clean launcher route require-launcher gate

# Guard for every target that sources runtime config (SERVICES/MCP/
# models) from config.toml: the launcher binary MUST exist, and `config get`
# MUST work — otherwise the $(shell …) sourcing above yields silently-empty
# values. Fail loudly instead.
require-launcher:
	@[ -x "$(PIX_BIN)" ] || { \
		echo "ERROR: $(PIX_BIN) not found. Runtime config (SERVICES/MCP/models) is"; \
		echo "       sourced from ~/.config/pix/config.toml via 'pix config get',"; \
		echo "       so the launcher must be built first: make launcher  (or: make install)"; \
		exit 1; }
	@"$(PIX_BIN)" config get services >/dev/null || { \
		echo "ERROR: '$(PIX_BIN) config get' failed — cannot source runtime config from config.toml."; \
		echo "       Run 'pix config show' to diagnose (bad profile? corrupt config.toml?)."; \
		exit 1; }


help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Runtime, routing, agent, and parallel-task commands live in the launcher,"
	@echo "not make:  pix help --all  (e.g. pix route compile,"
	@echo "pix agent ls, pix task new)."

build: ## Build the pix image from the DHI base
	docker build -t $(IMAGE) .

# CRITICAL: sbx caches a materialized image PER TAG. With a fixed tag (:0.0.1),
# `sbx run` keeps booting the first-cached copy and silently ignores every
# reload — verified by creating sandboxes and finding stale extensions. So we
# tag each build uniquely, load that, and `make run` pins --template to it.
# Old local-*/$(VERSION) templates are pruned so the store doesn't grow.
# (These comments live ABOVE the recipe so make doesn't echo them to the terminal.)
load: build ## Build + load the image into sbx under a UNIQUE tag, so `make run` uses this exact build
	@set -e; TS="local-$$(date +%s)"; T="docker.io/$(DOCKER_USER)/pix:$$TS"; \
	docker tag $(IMAGE) "$$T"; \
	docker save "$$T" -o out/pix.tar; \
	for id in $$(sbx template ls 2>/dev/null | awk '$$1=="docker.io/$(DOCKER_USER)/pix" && ($$2=="$(VERSION)" || $$2 ~ /^local-/){print $$3}'); do sbx template rm "$$id" >/dev/null 2>&1 || true; done; \
	sbx template load out/pix.tar; \
	rm -f out/pix.tar; docker rmi "$$T" >/dev/null 2>&1 || true; \
	echo "$$TS" > out/.local-image-tag; \
	REF="docker.io/$(DOCKER_USER)/pix:$$TS"; \
	echo "Loaded image:  $$REF"; \
	echo ""; \
	echo "Run this exact build (recreates the sandbox so the new image takes effect):"; \
	echo "  pix run --replace --template $$REF     # from ANY directory (5-worktree friendly)"; \
	echo "  sbx rm -f $(NAME) && make run               # dev flow from this checkout (live skills + MCP)"

publish: build ## Push the built image to the registry as :$(VERSION) and :latest (run `docker login` first)
	docker push $(IMAGE)
	docker tag $(IMAGE) $(LATEST)
	docker push $(LATEST)
	@echo "Published $(IMAGE) and $(LATEST)."
	@echo "  Discoverability tag: $(LATEST) (for manual docker pull / Hub browsing)."
	@echo "  Kit pins :$(VERSION), so consumers + local runs resolve the version (no re-pull)."
	@echo "  Consumers: sbx run pix --kit \"git+https://github.com/$(DOCKER_USER)/pix.git#dir=pi-kit\""

# The SAME gate CI runs (.github/workflows/test.yml, job `gate`) — build, vet,
# NON-race go test, node --test, tsc, open-core, and the rename guard once it
# exists — with per-segment + per-test timings and an absolute 12s ceiling.
# `go test -race` is deliberately NOT here: it is CI's separate untimed job.
gate: ## Run the fast PR gate locally (timed, 12s absolute budget) — same script CI runs
	@bash scripts/gate.sh

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
# (pix-<dir>) is safe. Non-default names must follow the same rule.
NAME ?= pix-pix
run: require-launcher ## Launch a pix sandbox NAME. If NAME is stopped it's recreated (workspace + .pi-sessions are host-mounted, so nothing is lost); if it's already running this refuses rather than clobber a live session. `make run NAME=pix-2` opens a second parallel sandbox in another window. (Kit-defined agents can't be re-attached, hence recreate.)
	@status=$$(sbx ls 2>/dev/null | awk -v n="$(NAME)" '$$1==n{print $$3}'); \
	if [ "$$status" = "running" ]; then \
		echo "ERROR: sandbox $(NAME) is already running (a live pi). Use a different name (make run NAME=pix-2) or 'sbx rm -f $(NAME)' first."; exit 1; \
	fi; \
	if [ -n "$$status" ]; then \
		echo "(sandbox $(NAME) exists [$$status] — recreating; workspace + .pi-sessions persist on the host)"; \
		sbx rm -f $(NAME) >/dev/null 2>&1 || true; \
	fi; \
	TAG=$$(cat out/.local-image-tag 2>/dev/null || true); \
	[ -n "$$TAG" ] && echo "(new sandbox $(NAME), local build :$$TAG)" || echo "(new sandbox $(NAME), kit-pinned image)"; \
	mkdir -p .pix && echo "$(OLLAMA_BRIDGE_MODEL)" > .pix/ollama-bridge.model; \
	exec sbx run pix --name $(NAME) $${TAG:+--template docker.io/$(DOCKER_USER)/pix:$$TAG} --kit $(KIT) $(MCP_FLAGS) . -- $(DEV_SKILLS)

# Run the latest PUBLISHED image straight off the git-hosted kit — the true
# consumer path, no local repo needed. Every push to main auto-publishes a NEW
# version (CI stamps 0.0.<run_number>, see .github/workflows/publish.yml) and
# commits the bump into pi-kit/spec.yaml on main. Since the kit reads spec.yaml
# from main, this always pins a version sbx has NEVER cached → a fresh pull with
# no --template override and no `sbx template rm` dance. Wait for CI green first.
run-published: ## Run the latest PUBLISHED image via the git kit (always fresh — every push is a new version tag; no eviction needed). `make run` = local build.
	-sbx rm -f pix-published >/dev/null 2>&1
	@sbx run pix --name pix-published --kit "git+https://github.com/$(DOCKER_USER)/pix.git#dir=pi-kit"

run-no-mcp: ## Launch with NO MCP servers attached (debugging MCP setup failures)
	@sbx run pix --kit $(KIT) .

launcher: ## Build BOTH host binaries (out/pix launcher + out/pix-host services), version-stamped (local builds stamp $(VERSION)+local so the launcher uses the local kit, not a nonexistent v$(VERSION) tag)
	(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix ./cmd/pix)
	(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix-host .)
	@echo "Built out/pix + out/pix-host (version $(LAUNCHER_VERSION))."
	@echo "Install both: ln -sf $(CURDIR)/out/pix ~/.local/bin/pix && ln -sf $(CURDIR)/out/pix-host ~/.local/bin/pix-host"

memory-serve: ## Build + run just the memory service (JSON-RPC :11435) from pix-host
	(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix-host .) && exec ./out/pix-host memory

mcp-auth: ## (Re)authorize the remote OAuth MCP servers (opine/granola/notion/atlassian). Run this when standup/refresh reports them "not in the gateway" — sbx's hosted MCP OAuth creds do NOT persist reliably across sessions/daemon restarts (they silently drop to "Not Found"), so re-establishing them is a recurring chore until sbx fixes it. Opens a browser per server.
	@command -v sbx >/dev/null 2>&1 || { echo "ERROR: sbx not found"; exit 1; }
	@echo "1/3 refreshing the control-plane session (sbx login)…"
	sbx login
	@echo "2/3 authorizing all registered remote OAuth servers…"
	sbx mcp auth --all
	@echo "3/3 status:"
	-sbx mcp auth status --all
	@echo ""
	@echo "If all show authorized, attach them LIVE to a running sandbox (no recreate):"
	@echo "  pix mcp load <server>   # or: sbx mcp load <server> --sandbox pix-pix"
	@echo "A fresh 'make run' also picks them up."

mcp-register: require-launcher ## Register the local stdio MCP servers you use (the mcp list in config.toml) with sbx's local data-plane gateway (no SBX_MCP_URL needed). The gateway runs each as `op run --no-masking --env-file=config/op-refs.env -- pix-host <name>`, so creds come from 1Password at spawn (nothing stored in the registration). Needs op + config/op-refs.env.
	@command -v sbx >/dev/null 2>&1 || { echo "ERROR: sbx not found"; exit 1; }
	@[ -n "$(strip $(REGISTER))" ] || { echo "Nothing to register: no local stdio servers ($(LOCAL_STDIO_MCP)) are in MCP. Run: pix config set mcp <name>."; exit 0; }
	@[ -n "$(OP_BIN)" ] || { echo "ERROR: 1Password CLI 'op' not found on PATH."; exit 1; }
	@[ -f "$(OP_REFS)" ] || { echo "ERROR: $(OP_REFS) missing. Create it:  cp config/op-refs.env.example config/op-refs.env  then fill in your refs."; exit 1; }
	@(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix-host .)
	@BIN="$(CURDIR)/out/pix-host"; \
	for s in $(REGISTER); do \
		case "$$s" in \
		google-workspace) \
			: 'Google Workspace has ONE writer: the launcher transaction. It authorizes,'; \
			: 'proves the headless spawn, then registers. Never hand-rolled here.'; \
			echo "  google-workspace: run 'pix gworkspace setup' (it registers after proving the spawn)" ;; \
		*) \
			sbx mcp add $$s --command "$(OP_BIN)" \
				--args run --args --no-masking --args "--env-file=$(OP_REFS)" --args -- --args "$$BIN" --args mcp --args "$$s" \
				&& echo "  registered: $$s" || echo "  FAILED to register: $$s" ;; \
		esac; \
	done
	@echo "Verify: sbx mcp ls"
	@echo "Attach: registration is NOT enough — a sandbox gets a server at CREATE via --static-mcp."
	@echo "        \`make run\` does this for you (MCP=$(MCP) from config.toml, passing --static-mcp for each)."
	@echo "        To attach one to an ALREADY-RUNNING sandbox live (no recreate): pix mcp load <name>"
	@echo "Note: each server resolves its creds from config/op-refs.env via op run when the gateway spawns it — make sure those refs are filled + valid."

serve: require-launcher ## Start the host services named in SERVICES (config.toml `services`): memory :11435. MCP servers (slack, google-workspace) are run by the sbx gateway — see `make mcp-register`. Ctrl-C stops all.
	@echo "Host services [$(SERVICES)] — sandboxes reach these on host.docker.internal. Ctrl-C stops all."
	@(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix-host .) || { echo "go build failed (pix-host)"; exit 1; }
	@exec env $(SERVE_ENV) MEMORY_WATCHER_MODEL=$(MEMORY_WATCHER_MODEL) MEMORY_EMBED_MODEL=$(MEMORY_EMBED_MODEL) out/pix-host serve $(SERVICES)

# route is MAINTAINER tooling for the model router, run from the repo (it reads
# services/host/routing/). It is NOT part of the consumer surface: `route` is
# deliberately NOT a `pix` command — it lives here in the Makefile, invoking
# the repo-built pix-host backend. See the `model-refresh` skill +
# docs/design/routing.md. Scores are hand-maintained in
# services/host/routing/defaults/scorecard.json — edit it, then `make route
# ARGS=compile` (or `pix route compile`).
# Bare `make route` defaults to the safe, read-only `show` (the scorecard /
# resolved table) so it never errors without ARGS.
route: ## Model router (maintainer): make route ARGS="show" | "models" | "compile" | "pick <intent>"
	@(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix-host .) && ./out/pix-host route $(if $(strip $(ARGS)),$(ARGS),show)

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
	echo "Config (config.toml via 'pix config get' — the single source of truth):"; \
	printf "  %-9s %s\n" "SERVICES" "$(SERVICES)   (make serve runs these)"; \
	printf "  %-9s %s\n" "MCP"      "$(if $(strip $(MCP)),$(MCP),<empty: none configured>)   (configured MCPs preload at sandbox creation via make run)"; \
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
	printf "  %-7s setup: %-30s serving: %s\n" "gwork" "$$(command -v gog >/dev/null 2>&1 && echo 'dependency installed' || echo 'TODO: pix gworkspace setup')" "MCP via gateway (pix gworkspace setup)"; \
	printf "  %-7s setup: %-30s serving: %s\n" "memory" "watcher+embed above" ":11435 $$(port 11435) (capture needs the watcher model)"; \
	echo ""; \
	echo "MCP servers (local stdio, run by the sbx gateway — register with 'make mcp-register', attach with 'make run'):"; \
	reg() { sbx mcp ls 2>/dev/null | grep -qw "$$1" && echo "registered" || echo "TODO: make mcp-register"; }; \
	printf "  %-7s %-14s %s\n" "slack"  "$$(reg slack)"    "$(if $(filter slack,$(MCP)),auto-attached on make run,NOT in MCP — 'pix config set mcp slack' to use)"; \
	printf "  %-7s %-14s %s\n" "gwork" "$$(reg google-workspace)" "$(if $(filter google-workspace,$(MCP)),auto-attached on make run,NOT set up — 'pix gworkspace setup' to use)"; \
	echo "  gateway catalog (atlassian/notion/granola/linear/...): sbx mcp add … then pix config set mcp <name>"; \
	echo ""; \
	echo "All of the above is configured in ~/.config/pix/config.toml (pix config set). Start it: make serve (host) + make run (sandbox)."

pack: ## Package the kit as a distributable zip
	sbx kit pack $(KIT) -o out/pix-kit.zip

install: launcher ## Build + put the Go binaries (out/pix launcher + out/pix-host) on your PATH (~/.local/bin)
	mkdir -p $(HOME)/.local/bin
	ln -sf $(CURDIR)/out/pix $(HOME)/.local/bin/pix
	ln -sf $(CURDIR)/out/pix-host $(HOME)/.local/bin/pix-host
	@echo "Installed: pix -> $(CURDIR)/out/pix"
	@echo "Installed: pix-host -> $(CURDIR)/out/pix-host"
	@# Drop the man page on the user manpath too (bonus; the binary embed is the
	@# guarantee, so `pix man` works with or without this). No sudo.
	mkdir -p $(HOME)/.local/share/man/man1
	cp services/host/cmd/pix/pix.1 $(HOME)/.local/share/man/man1/pix.1
	@echo "Installed: man page -> $(HOME)/.local/share/man/man1/pix.1"
	@manpath 2>/dev/null | tr ':' '\n' | grep -qx "$(HOME)/.local/share/man" \
		|| echo "Tip: add ~/.local/share/man to MANPATH for \`man pix\` (or just use \`pix man\`)."
	@echo "Runtime config lives in ~/.config/pix/config.toml — manage it with"
	@echo "'pix config set <key> <value>' (or 'pix setup' for the guided flow)."
	@echo "Ensure ~/.local/bin is on your PATH, then: cd <any project> && pix"

clean: ## Remove the built image
	-docker rmi $(IMAGE)
