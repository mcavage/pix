# Set DOCKER_USER to your Docker Hub namespace before `make publish`.
# VERSION is ONE Pix release version, PINNED (never `latest`): Docker re-pulls
# `:latest` on every run even when the image is already loaded, so `make load`
# would be ignored. A pinned tag gets IfNotPresent semantics — use the loaded
# local build if present, else pull once. Keep in sync with `version` in
# package.json and the `image:` refs in pi-kit/spec.yaml.
#
# pix-agent (the sandbox image, images/agent/Dockerfile) and pix-memory (the
# memory MCP service, services/memory/Dockerfile) are INDEPENDENT images with
# their own Dockerfiles, build targets, and immutable digests — see
# docs/design/pix-v2-architecture.md §3. Both are tagged at the SAME Pix
# VERSION: the release manifest binds one Pix version to both digests, not two
# independently numbered images.
DOCKER_USER  ?= mcavage
VERSION      ?= 0.1.71
# LAUNCHER_VERSION stamps the pix binary. A LOCAL build derives a NEXT-PATCH
# prerelease (X.Y.(Z+1)-beta.g<sha7>[.dirty.<12hex>], scripts/release/derive-build-version.sh)
# from the committed package.json base plus this checkout's git state, so a
# release stack (pinned at the clean $(VERSION)) and a dev stack (pinned at
# this derived identity) coexist without either resolving to, or being
# mistaken for, the other's tag. `launcher.IsReleased` (services/host/launcher/released.go)
# still correctly calls this UNRELEASED — it is never a bare X.Y.Z — so it
# uses the local checkout kit instead of pinning a nonexistent v$(VERSION) tag.
# A CI RELEASE build overrides this to a clean X.Y.Z (LAUNCHER_VERSION=$(VERSION)):
# `?=` never evaluates the default once the variable already has a value from
# the command line or environment, so a release build never shells out to
# derive anything.
#
# A FAILED derivation stops the build. $(shell ...) reports a failing command
# as the empty string, and an empty LAUNCHER_VERSION is silently poisonous: it
# stamps a versionless binary, names the runtime archive
# out/pix-runtime-.tar.gz, tags both images `pix-agent:` and binds a manifest
# to that nothing. The script itself already refuses loudly (nonzero exit, a
# reason on stderr, no stdout), so "empty" IS "it refused" and the only honest
# response is to abort here with the reason and the override. DERIVE_VERSION_SH
# is a variable so a test can point this at a script that fails on purpose.
DERIVE_VERSION_SH ?= scripts/release/derive-build-version.sh
launcher-version-or-die = $(if $(strip $(1)),$(1),$(error make: could not derive the local build version: $(DERIVE_VERSION_SH) failed (its own reason is on stderr above). Fix that, or build with an explicit identity: make $(MAKECMDGOALS) LAUNCHER_VERSION=X.Y.Z))
LAUNCHER_VERSION ?= $(call launcher-version-or-die,$(shell $(DERIVE_VERSION_SH)))
AGENT_DOCKERFILE  ?= images/agent/Dockerfile
AGENT_IMAGE       ?= docker.io/$(DOCKER_USER)/pix-agent:$(VERSION)
AGENT_LATEST      ?= docker.io/$(DOCKER_USER)/pix-agent:latest
MEMORY_DOCKERFILE ?= services/memory/Dockerfile
MEMORY_IMAGE      ?= docker.io/$(DOCKER_USER)/pix-memory:$(VERSION)
MEMORY_LATEST     ?= docker.io/$(DOCKER_USER)/pix-memory:latest

# The LOCAL build identity, shared by the binary, the runtime archive, the
# release manifest AND both images. A dev stack and a release stack coexist
# on one host only if EVERY artifact they own carries the same, distinct
# identity: a `make build` that tagged pix-agent :$(VERSION) would overwrite
# the published tag the release stack pins, and `make release-manifest`
# would then bind a $(LAUNCHER_VERSION) manifest to a digest sitting under
# the release stack's tag. So local builds tag :$(LAUNCHER_VERSION) and only
# the publish targets (and release CI, which passes LAUNCHER_VERSION=$(VERSION)
# so the two collapse into one clean tag) ever touch :$(VERSION).
LOCAL_AGENT_IMAGE  ?= docker.io/$(DOCKER_USER)/pix-agent:$(LAUNCHER_VERSION)
LOCAL_MEMORY_IMAGE ?= docker.io/$(DOCKER_USER)/pix-memory:$(LAUNCHER_VERSION)

# WORKTREE_HASH identifies THIS checkout, and nothing else, the same way the
# launcher identifies a PIX_HOME: the first 12 hex characters of the sha256
# of the canonical (symlink-resolved) directory path — services/host/stack's
# CanonicalPath + HashPrefix, in shell. `make load` composes its unique tag
# from it and prunes ONLY tags carrying it, so a five-worktree dev machine
# never has one worktree's `make load` delete another worktree's freshly
# loaded template out from under a running sandbox.
WORKTREE_HASH ?= $(shell printf '%s' "$$(cd $(CURDIR) && pwd -P)" | { command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256; } | cut -c1-12)
KIT         ?= ./pi-kit
# Dev mode (Mode B): `make run` launches from the repo, so load skills LIVE from the
# host tree instead of the copies baked into the image — edit a SKILL.md, /reload in
# pi, and it's live, no rebuild. `--no-skills` turns off baked discovery; `--skill
# <root>` recurses for SKILL.md. Company/private context is NOT compiled into
# the image — it lives in an environment (~/.pix/envs/<name>, see
# docs/design/pix-v2-surface.md), and host-executing integrations are declared
# directly in that environment's .sbxenv.yaml and run through the sbx MCP
# Gateway. Consumers who `sbx run --kit git+...` never hit this
# target, so they get the baked set (Mode A). See AGENTS.md.
DEV_SKILLS = --no-skills --skill $(CURDIR)/skills
# The launcher binary this Makefile drives. There is no `pix config get`:
# machine defaults live in ~/.pix/config.toml with ONE named writer each
# (`pix env default`, `pix setup`, `pix secret set`), so nothing here shells
# out to read configuration — a Make target that needs a value takes it as a
# variable override.
PIX_BIN ?= $(CURDIR)/out/pix

# The local model `make run` hands the in-sandbox ollama bridge. Overridable
# on the command line (`make run OLLAMA_BRIDGE_MODEL=qwen3:8b`); it is not
# read from any config file.
OLLAMA_BRIDGE_MODEL ?= qwen3:4b

# out/ is gitignored, so it's absent on a fresh clone. Several targets (load,
# launcher, runtime-archive, release-manifest, bundle) write into it and would otherwise fail
# with "invalid output path: stat out: no such file or directory". Create it once
# at parse time so every target can rely on it.
$(shell mkdir -p out)

.PHONY: help build build-agent build-memory load publish publish-agent publish-memory validate inspect run run-published run-no-mcp secrets install clean launcher require-launcher gate runtime-archive release-manifest bundle

# Bare `make` builds the launcher binary (the one thing require-launcher
# demands as a prerequisite for `make run`), so a dev iterating on the
# host never hits the "must be built first" guard on a fresh checkout. Pin the
# default goal explicitly: require-launcher is the first target, so without
# this GNU make would make the guard the default and error out.
.DEFAULT_GOAL := launcher

# Guard for every target that execs the launcher: the binary MUST exist.
# It does NOT probe configuration — `pix config get` does not exist in v2 —
# so this checks exactly one thing and says exactly one thing.
require-launcher:
	@[ -x "$(PIX_BIN)" ] || { \
		echo "ERROR: $(PIX_BIN) not found. Build it first: make launcher  (or: make install)"; \
		exit 1; }


help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Everything a user runs lives in the launcher, not make:"
	@echo "  pix help --all   (pix setup, pix run, pix task new, pix env, pix doctor)"

build: build-agent ## Alias for build-agent (the sandbox image consumers pull; pix-memory is built separately, see build-memory)

build-agent: ## Build the pix-agent sandbox image from images/agent/Dockerfile (DHI Node/Debian base), tagged at the LOCAL build identity $(LAUNCHER_VERSION)
	docker build -f $(AGENT_DOCKERFILE) -t $(LOCAL_AGENT_IMAGE) .

build-memory: ## Build the pix-memory MCP service image from services/memory/Dockerfile (DHI Go builder + minimal DHI runtime), tagged at $(LAUNCHER_VERSION) with a MATCHING VERSION build arg
	docker build -f $(MEMORY_DOCKERFILE) --build-arg VERSION=$(LAUNCHER_VERSION) -t $(LOCAL_MEMORY_IMAGE) services/memory

# CRITICAL: sbx caches a materialized image PER TAG. With a fixed tag (:0.0.1),
# `sbx run` keeps booting the first-cached copy and silently ignores every
# reload — verified by creating sandboxes and finding stale extensions. So we
# tag each build uniquely, load that, and `make run` pins --template to it.
# Old templates from THIS WORKTREE (local-$(WORKTREE_HASH)-*) are pruned so
# the store doesn't grow. The prune is worktree-scoped on purpose: a bare
# /^local-/ match (and a $(VERSION) match) deleted templates belonging to
# OTHER checkouts on a multi-worktree machine, including ones a live sandbox
# in another window was created from. A worktree only ever removes its own.
# (These comments live ABOVE the recipe so make doesn't echo them to the terminal.)
# load/run only ever concern pix-agent: pix-memory is a plain Docker container
# (see docs/design/pix-v2-architecture.md §9), never an sbx sandbox template.
load: build-agent ## Build + load the pix-agent image into sbx under a UNIQUE, WORKTREE-SCOPED tag, so `make run` uses this exact build
	@set -e; TS="local-$(WORKTREE_HASH)-$$(date +%s)"; T="docker.io/$(DOCKER_USER)/pix-agent:$$TS"; \
	docker tag $(LOCAL_AGENT_IMAGE) "$$T"; \
	docker save "$$T" -o out/pix.tar; \
	for id in $$(sbx template ls 2>/dev/null | awk '$$1=="docker.io/$(DOCKER_USER)/pix-agent" && $$2 ~ /^local-$(WORKTREE_HASH)-/{print $$3}'); do sbx template rm "$$id" >/dev/null 2>&1 || true; done; \
	sbx template load out/pix.tar; \
	rm -f out/pix.tar; docker rmi "$$T" >/dev/null 2>&1 || true; \
	echo "$$TS" > out/.local-image-tag; \
	REF="docker.io/$(DOCKER_USER)/pix-agent:$$TS"; \
	echo "Loaded image:  $$REF"; \
	echo ""; \
	echo "Run this exact build (recreates the sandbox so the new image takes effect):"; \
	echo "  pix rm <name> && pix run --template $$REF     # from ANY directory (5-worktree friendly)"; \
	echo "  make run                                       # dev flow from this checkout (live skills + MCP)"

publish: publish-agent publish-memory ## Push BOTH pix-agent and pix-memory to the registry

# Publish builds the CLEAN, published identity directly — it deliberately
# does NOT depend on build-agent/build-memory, which tag the LOCAL
# $(LAUNCHER_VERSION) identity. Release CI sets LAUNCHER_VERSION=$(VERSION)
# so the two are the same string there; on a dev machine they are not, and a
# publish must never push whatever a developer's dirty tree happened to build.
publish-agent: ## Push the pix-agent image to the registry as :$(VERSION) and :latest (run `docker login` first)
	docker build -f $(AGENT_DOCKERFILE) -t $(AGENT_IMAGE) .
	docker push $(AGENT_IMAGE)
	docker tag $(AGENT_IMAGE) $(AGENT_LATEST)
	docker push $(AGENT_LATEST)
	@echo "Published $(AGENT_IMAGE) and $(AGENT_LATEST)."
	@echo "  Discoverability tag: $(AGENT_LATEST) (for manual docker pull / Hub browsing)."
	@echo "  Kit pins :$(VERSION), so consumers + local runs resolve the version (no re-pull)."
	@echo "  Consumers: sbx run pix --kit \"git+https://github.com/$(DOCKER_USER)/pix.git#dir=pi-kit\""

publish-memory: ## Push the pix-memory image to the registry as :$(VERSION) and :latest (run `docker login` first)
	docker build -f $(MEMORY_DOCKERFILE) --build-arg VERSION=$(VERSION) -t $(MEMORY_IMAGE) services/memory
	docker push $(MEMORY_IMAGE)
	docker tag $(MEMORY_IMAGE) $(MEMORY_LATEST)
	docker push $(MEMORY_LATEST)
	@echo "Published $(MEMORY_IMAGE) and $(MEMORY_LATEST)."

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

secrets: ## Show the per-PIX_HOME 1Password refs Pix can scope to each sandbox
	@echo "Store 1Password references in this stack's secrets.env:"
	@echo '  pix secret set ANTHROPIC_API_KEY op://vault/item/field'
	@echo '  pix secret set OPENAI_API_KEY    op://vault/item/field'
	@echo '  pix secret set GEMINI_API_KEY    op://vault/item/field'
	@echo '  pix secret set GITHUB_TOKEN      op://vault/item/field'
	@echo "Pix resolves configured refs on each run and refreshes sandbox-scoped sbx secrets."
	@echo "Host-global sbx secrets are ignored and never removed automatically."

# NAME is an OPTIONAL override. It is deliberately EMPTY by default: the
# launcher derives the sandbox name itself (services/host/sandbox.Name —
# pix-<stack-id>-<basename>-<workspace-digest>), which is what makes two
# PIX_HOMEs coexist on one host. A hardcoded `NAME ?= pix-pix` here pinned
# every dev run of every worktree, under every PIX_HOME, to ONE unscoped
# name: the second stack's `make run` found the first stack's sandbox and
# either refused or attached to it.
#
# NAME, when set, is a short logical name. The launcher validates and expands
# it to pix-<stack-id>-<name>; make never tries to interpret that scoped name.
NAME ?=
run: require-launcher ## Launch this checkout with its stack-scoped sandbox name; NAME is an optional short logical override
	@N="$(NAME)"; \
	TAG=$$(cat out/.local-image-tag 2>/dev/null || true); \
	LABEL="$${N:-the launcher-derived stack-scoped name}"; \
	[ -n "$$TAG" ] && echo "(sandbox $$LABEL, local build :$$TAG)" || echo "(sandbox $$LABEL, kit-pinned image)"; \
	mkdir -p .pix && echo "$(OLLAMA_BRIDGE_MODEL)" > .pix/ollama-bridge.model; \
	exec "$(PIX_BIN)" run --dev $${N:+--name "$$N"} $${TAG:+--template docker.io/$(DOCKER_USER)/pix-agent:$$TAG} .

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

launcher: ## Build the ONE pix binary (out/pix), version-stamped (local builds derive a next-patch -beta.g<sha7> prerelease, see LAUNCHER_VERSION above, so the launcher uses the local kit, not a nonexistent v$(VERSION) tag). services/host/cmd/pix is the only build target, and the only binary a release ships.
	(cd services/host && go build -ldflags "-X main.version=$(LAUNCHER_VERSION)" -o $(CURDIR)/out/pix ./cmd/pix)
	@echo "Built out/pix (version $(LAUNCHER_VERSION))."
	@echo "Install: ln -sf $(CURDIR)/out/pix ~/.local/bin/pix"

runtime-archive: ## Build the runtime archive (skills/agents/settings/keybindings/themes, canonical runtime/<version>/ layout) WITHOUT touching the live repo layout dev `make run` reads. Named at $(LAUNCHER_VERSION) — the SAME identity the launcher binary stamps — so a local install's runtime dir always matches the binary that reads it; release CI passes the clean $(VERSION) directly to this same script instead (see .github/workflows/publish.yml), never through this target.
	bash scripts/release/build-runtime-archive.sh $(LAUNCHER_VERSION) out/pix-runtime-$(LAUNCHER_VERSION).tar.gz

# Binds ONE Pix version to the pix-agent digest, the pix-memory digest, the
# runtime archive digest, and the kit revision (docs/design/pix-v2-architecture.md
# §3). Digests are read from `docker inspect` on the just-built local images —
# this is the LOCAL/dev shape; CI's publish workflow binds the PUBLISHED
# (pushed, multi-arch) digests instead, which only exist after a real push.
release-manifest: build-agent build-memory runtime-archive ## Emit out/release-manifest.json binding version + both image digests + runtime digest + kit revision. EVERY artifact shares ONE identity, $(LAUNCHER_VERSION): the launcher binary, the runtime archive, the manifest "version", and both locally built image tags. That is the whole point of the derived local version — a beta bundle is internally consistent and never binds itself to a digest sitting under the published :$(VERSION) tag. Release CI sets LAUNCHER_VERSION=$(VERSION) and publishes the clean semver instead.
	@AGENT_DIGEST=$$(docker image inspect $(LOCAL_AGENT_IMAGE) --format '{{index .RepoDigests 0}}' 2>/dev/null | sed 's/^.*@//'); \
	if [ -z "$$AGENT_DIGEST" ]; then AGENT_DIGEST="sha256:$$(docker image inspect $(LOCAL_AGENT_IMAGE) --format '{{.Id}}' | sed 's/^sha256://')"; fi; \
	MEMORY_DIGEST=$$(docker image inspect $(LOCAL_MEMORY_IMAGE) --format '{{index .RepoDigests 0}}' 2>/dev/null | sed 's/^.*@//'); \
	if [ -z "$$MEMORY_DIGEST" ]; then MEMORY_DIGEST="sha256:$$(docker image inspect $(LOCAL_MEMORY_IMAGE) --format '{{.Id}}' | sed 's/^sha256://')"; fi; \
	node scripts/release/emit-manifest.mjs \
		--version $(LAUNCHER_VERSION) \
		--agent-digest "$$AGENT_DIGEST" \
		--memory-digest "$$MEMORY_DIGEST" \
		--runtime-archive out/pix-runtime-$(LAUNCHER_VERSION).tar.gz \
		--write out/release-manifest.json

bundle: launcher release-manifest ## Assemble the installable release bundle in out/: the ONE pix binary + release-manifest.json + pix-runtime-$(LAUNCHER_VERSION).tar.gz
	@ls -l out/pix out/release-manifest.json out/pix-runtime-$(LAUNCHER_VERSION).tar.gz

install: bundle ## Build the release bundle and put the ONE pix binary on your PATH (~/.local/bin)
	mkdir -p $(HOME)/.local/bin
	ln -sf $(CURDIR)/out/pix $(HOME)/.local/bin/pix
	@echo "Installed: pix -> $(CURDIR)/out/pix"
	@# `pix setup` discovers the release bundle NEXT TO THE RESOLVED binary, so
	@# this symlink is fine: it resolves into out/, where release-manifest.json
	@# and pix-runtime-$(LAUNCHER_VERSION).tar.gz were just written by release-manifest.
	@echo "Bundle:    out/release-manifest.json + out/pix-runtime-$(LAUNCHER_VERSION).tar.gz"
	@echo "Next:      pix setup   (installs the runtime under ~/.pix and reconciles pix-memory)"
	@echo "Ensure ~/.local/bin is on your PATH, then: cd <any project> && pix"

clean: ## Remove this checkout's locally built images (the $(LAUNCHER_VERSION) identity), never another stack's published :$(VERSION) tags
	-docker rmi $(LOCAL_AGENT_IMAGE)
	-docker rmi $(LOCAL_MEMORY_IMAGE)
