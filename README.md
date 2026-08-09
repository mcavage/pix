# Pix

Pix is an opinionated, Docker-sandboxed distribution of the
[pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent).
You type `pix`, enter an agent session isolated to your current directory, and
exit with your host machine untouched.

Because the sandbox is the safety boundary, the agent does not stop to ask
permission for each shell command. It can build, test, review, and open a pull
request in one run.

## Install and setup

<!-- PIX_PRIMARY_PATH_START -->

```bash
brew install mcavage/tap/pix
pix setup
```

Do not use bare `brew install pix`: Homebrew does not know a formula by that
name and may suggest `pixi` instead. The `mcavage/tap/` qualifier is required.
`pix setup` configures model providers and launches your first sandbox. Private
skills, knowledge, and integrations are added as git-backed packs, never as
edits to this repo:

```bash
pix setup --pack 'git+https://github.com/your-org/work-pack.git#ref=main'
```

See [docs/design/packs.md](docs/design/packs.md) for what a pack can carry and
how host-executing hooks are reviewed before they run.

<!-- PIX_PRIMARY_PATH_END -->

## Daily use

```bash
pix                  # launch or reattach the sandbox for this directory
pix run [DIR]        # the same thing, said explicitly
pix status           # what is up, what is down, what is next
pix ls               # list your sandboxes;  pix rm <name>  removes one
pix doctor           # readiness evidence and the exact fix commands
pix task new <name>  # an isolated clone plus sandbox for parallel work
pix memory           # recall, remember, forget, learnings, stats
pix help --all       # complete command map
```

Plain `pix` launches or reattaches the sandbox for the current directory. When
stdin is not a terminal (scripts, pipes, CI) it prints status instead, so
nothing launches by accident. `pix status` is the explicit spelling.

Host services start lazily. Install them as a login service with `pix serve
install`. Runtime config is managed with `pix config`, not by editing TOML.

## In the sandbox

pi runs in fullscreen TUI mode:

- The transcript scrolls on its own (mouse wheel, PageUp) instead of the
  terminal snapping to the bottom while output streams.
- Drag-selecting text copies it to the system clipboard.
- Printed links are clickable and open in your host browser.

## What you get

- **Isolation.** A disposable Docker Sandbox per directory. Setup starts with
  open outbound networking, which you can tighten later with `sbx policy`.
- **Models by intent.** Sessions and subagent roles resolve through a router by
  intent (code, review, strategy) rather than a pinned model name, so you never
  pick a model per task. See [docs/design/routing.md](docs/design/routing.md).
- **Cross-vendor review.** Code-producing workflows are reviewed by a different
  vendor than wrote the code. Different training, different blind spots.
- **Memory.** A host service keeps durable facts in SQLite with full-text and
  vector search, and recalls them into context without rewriting the provider
  cache prefix.
- **Parallel work.** Each `pix task` gets its own clone and sandbox, so two
  agents never race in one working tree.
- **Web search.** Backends work with no key. Adding one improves results, and
  keys live in 1Password as references, never on disk:

  ```bash
  pix secret set PARALLEL_API_KEY op://vault/item/field
  pix secret sync
  ```

## Build from source

Install with Homebrew unless you are working on Pix itself. Building the image
needs a DHI-entitled Docker account:

```bash
make gate    # the fast test gate
make build
```

Image changes must then be loaded into the Docker Sandboxes image store from
the host. See [AGENTS.md](AGENTS.md) and
[docs/reference.md](docs/reference.md) for the maintainer architecture and the
full command reference.

## License

MIT. See [LICENSE](LICENSE).
