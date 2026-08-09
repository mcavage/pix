# Pix

Pix runs the [pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
inside a disposable Docker sandbox. You type `pix`, you get an agent session
scoped to the current directory, you exit, and your machine is untouched.

Because the sandbox is the boundary, the agent does not stop to ask permission
for each command. It can build, test, review its own work with a second model,
and open a pull request in one run.

## 1. Install

<!-- PIX_PRIMARY_PATH_START -->

```bash
brew install mcavage/tap/pix
pix setup
```

`pix setup` walks you through model keys, starts the host services, and launches
your first sandbox. It checks each thing, fixes only what it found broken, then
checks again, so nothing reports ready because a step claimed success.

Use the `mcavage/tap/` prefix. Bare `brew install pix` finds no such formula and
Homebrew will suggest `pixi`, which is a different tool.

<!-- PIX_PRIMARY_PATH_END -->

You need one model provider key to start. Add more later, and Pix will use
different vendors for different jobs (see Models below).

## 2. Use it

```bash
cd ~/code/my-project
pix
```

That is the whole loop. `pix` launches the sandbox for this directory, or
reattaches if one is already running. Exit the shell and the sandbox is torn
down.

```bash
pix status     # what is up, what is down, what is next
pix doctor     # the same, with evidence and the exact fix commands
pix ls         # your sandboxes;  pix rm <name>  removes one
```

In a script, a pipe, or CI, plain `pix` prints status instead of launching, so
nothing starts a sandbox by accident. `pix run` is the explicit spelling when
you want a launch regardless.

Inside the session: the transcript scrolls on its own (mouse wheel, PageUp),
drag-selecting text copies it to your clipboard, printed links open in your host
browser, and `/help` lists the skills available.

## 3. Make it yours

Three things you will actually want. None of them require a pack.

**Your own skills and standing instructions** live in one directory on your
host and load into every session:

```
~/.local/share/pix/context/
  AGENTS.md              # always-on instructions, in every session
  skills/<name>/SKILL.md # a named workflow, run with /skill:<name>
```

Create the file, start a session, it is there. Nothing to register or rebuild.

**MCP servers** are registered once on the host:

```bash
pix mcp add <name> --url <url>   # a hosted server
pix mcp auth <name>              # OAuth it, if it needs one
pix mcp ls                       # what the gateway knows about
```

Registration is host state, which is not the same as a session seeing the
tools. A sandbox picks up everything registered when it launches, so add
first, then start it. To catch a running one up: `pix rm <box>` then `pix`.

**API keys** are 1Password references, never values on disk:

```bash
pix secret set PARALLEL_API_KEY op://vault/item/field
pix secret sync
```

`pix secret` explains the two kinds of key it holds and what each one buys.

## 4. Share it (advanced)

A pack is the redistribution mechanism: one git repo carrying skills, knowledge,
MCP integrations and config, so a team or a second machine gets your whole setup
in one command.

```bash
pix pack use git+https://github.com/your-org/work-pack.git#ref=main
```

Reach for this when you are handing your setup to someone else. For your own
machine, section 3 is enough. Adopting a pack that runs code on your host stops
for a reviewable bill of materials first. See
[docs/design/packs.md](docs/design/packs.md).

## What you get

- **Isolation.** One disposable Docker Sandbox per directory. Networking starts
  open and can be tightened with `sbx policy`.
- **Models by intent.** Every role (the session itself, the reviewer, the
  researcher) resolves through a router by intent, not by a model name you
  pinned. Wire a vendor with `pix models add <provider>`; see `pix models` for
  the roster and [docs/design/routing.md](docs/design/routing.md) for how it
  picks.
- **Cross-vendor review.** Code gets reviewed by a different vendor than wrote
  it. Different training, different blind spots.
- **Web search, built in.** `web_search` and `fetch_content` work out of the
  box. Adding a `PARALLEL_API_KEY` upgrades the backend to Parallel, which Pix
  then prefers automatically.
- **Memory.** A host service keeps durable facts in SQLite with full-text and
  vector search and recalls them into context. `pix memory` to inspect it.
- **Parallel work.** `pix task new <name>` gets its own clone and sandbox, so
  two agents never race in one working tree.

## Working on Pix itself

Anything that maintains Pix is a `make` target, not a CLI command:

```bash
make gate      # the fast test gate
make build     # build the image (needs a DHI-entitled Docker account)
make load      # build and load it into the sandbox image store
make routing   # recompile the baked default model map
```

See [AGENTS.md](AGENTS.md) for the architecture and
[docs/reference.md](docs/reference.md) for the full command reference.

## License

MIT. See [LICENSE](LICENSE).
