# Pix

Pix runs the [pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
inside a Docker sandbox. You type `pix` in a directory, you get an agent session
scoped to that directory, you exit, and your host is untouched.

The sandbox is the boundary, so the agent does not stop to ask permission for
each command. It can build, test, review its own work with a second model, and
open a pull request in one run.

macOS only.

## 1. What you need first

Pix installs none of these. Install them yourself, then check each one.

| Thing | Required? | Install | Check |
| --- | --- | --- | --- |
| Homebrew | required | [brew.sh](https://brew.sh) | `brew --version` |
| `sbx`, Docker Sandboxes, plus a signed-in Docker account | required | `brew install docker/tap/sbx` | `sbx diagnose` |
| `op`, the 1Password CLI | required to add your own provider key | `brew install 1password-cli` | `op --version` |
| `gh`, the GitHub CLI | required to push or open PRs from a sandbox | `brew install gh` | `gh auth status` |
| Ollama | optional (see section 5) | [ollama.com](https://ollama.com) | `ollama --version` |

`sbx diagnose` prints a checklist. Every line must be a green tick, including
**Authentication**. If it is not authenticated, run `sbx login`.

A sandbox has no GitHub credential of its own. Give every sandbox one, once:

```bash
gh auth token | sbx secret set github
```

Service secrets are global by default, so this covers sandboxes you create
later. `sbx secret set github --sandbox <name>` scopes it to a single sandbox
instead, which means doing it again for the next one. Check what you have with
`sbx secret ls`: the `SCOPE` column shows `global` or a sandbox name.

Without it the agent can commit inside the sandbox but cannot push or open a
pull request.

Pix stores provider keys as 1Password references, never as values on disk. That
is the only reason `op` is on the required list. If a pack already supplies
credentialed inference (section 9), you never add a key and `op` stays optional.

## 2. Install

<!-- PIX_PRIMARY_PATH_START -->

```bash
brew install mcavage/tap/pix
pix setup
```

Use the `mcavage/tap/` prefix. Bare `brew install pix` matches no formula, and
Homebrew will suggest `pixi`, which is a different tool.

`pix setup` is guided and resumable. It checks every capability it owns, applies
only the gaps it verified, then checks again. It installs exactly three things:
the launchd agent for host services, any pack you asked for, and (only with
`--pull-models`) local Ollama weights. A gap it cannot repair is printed with
the exact command that fixes it.

<!-- PIX_PRIMARY_PATH_END -->

`pix setup` does not collect provider keys. Section 4 covers those.

## 3. How to tell it worked

```bash
pix doctor
```

Every check reports what it proved. On a host that is ready, the first line is:

```
✓ ready (1 not checkable from here)
```

On a host that is not ready yet, you get the gap and the command for it. This is
a real run against an empty config with no key and no pack:

```
✗ 1 of 3 required checks failed

✓ sbx        required 0.38
· pack       optional no active pack
    a pack carries skills, knowledge, MCP servers and config: pix pack use <path|owner/repo>
    evidence: no pack root configured
✗ providers  required none of anthropic, openai, google is set
    evidence: key store answered without anthropic, openai, google
✓ memory     required unit running (:11435)
✓ launchd    optional agent loaded
· mcp        optional none configured
· daemons    optional none declared

Fix:
    pix models add anthropic
```

`·` is its own, fifth verdict: verified, optional, and intentionally not
configured (no active pack, no MCP servers, no supervised daemons). It is
neither a gap (nothing to fix) nor a pass (nothing was exercised), so it never
appears in `Fix:` and never turns the headline red.

What each row means:

| Row | Required? | Proves |
| --- | --- | --- |
| `sbx` | required, always | the `sbx` CLI is installed |
| `providers` | required, always | at least one model provider is callable |
| `memory` | required once `services` lists it | the memory daemon answers on `:11435` |
| `pack` | never required | a pack is active |
| `launchd` | never required | the login agent is loaded |
| `mcp` | never required | each MCP server's host registration |
| `daemons` | never required | pack-declared services are answering |

Exit codes: `0` when nothing required is verifiably broken, `1` when a required
check verified a gap, `2` on a usage error. A check that could not be made is
never reported as a failure.

The short form:

```console
$ pix status
pix 0.1.42+local
✓ ready (1 not checkable from here)
  ready    sbx pack providers memory launchd daemons
  unknown  mcp
```

Two more direct checks:

```console
$ pix serve status
serve: running (pid 60013)
  memory (:11435): up

$ pix memory stats
active 1  facts 1  corrections 0  deleted 0
```

`pix doctor` and `pix status` read host state only. Neither can see inside a
running sandbox, so neither reports whether a live session has a given MCP
server's tools loaded.

## 4. Do I need a model provider API key?

Usually yes. `providers` is a required check. One key for any one of Anthropic,
OpenAI, or Google satisfies it. You do not need all three.

```bash
pix models add anthropic     # or: openai, google, ollama
```

That verb is the one place a 1Password reference is solicited. It wires the
provider and proves it with a live request.

The exception: a host whose configured backends carry their own auth, such as a
pack that ships an authenticated gateway. There `pix doctor` prints
`providers required no provider key needed` and means it.

To see what this host can call:

```console
$ pix models
MODEL                           BACKEND    SOURCE
anthropic/claude-opus-5         anthropic  machine config
...
ollama/qwen3.5:9b               ollama     machine config
```

`wired` means a probed backend can call it here. `unwired` means it is in the
catalog but not wired on this host. `pix models` alone prints which backends are
bound and which model the session will resolve to. `pix agent ls` prints the
subagent roster, each agent's resolved model, and the rule that picked it.

## 5. Is Ollama required?

No. Ollama is optional. Pix runs fine without it. Here is exactly what changes.

| Capability | With Ollama | Without Ollama |
| --- | --- | --- |
| Memory recall | vector ranking plus keyword search | FTS5 keyword search only |
| Automatic fact capture (`memory_capture experimental-auto`, opt-in) | works, a watcher model extracts facts | unavailable, there is no watcher model to extract facts |
| `/remember` and `/forget` | work | work (an explicit store, not an extraction) |
| Local model in the session | the model named by `ollama_bridge_model` | cloud models only |

Capture is `explicit` by default either way: Ollama being installed does not
turn automatic capture on by itself. It only decides whether
`experimental-auto` (if you opt in with `pix config set memory_capture
experimental-auto`) has a watcher model to run.

Three config keys name the models Pix will use if Ollama is present:

```bash
pix config get memory_embed_model      # recall ranking
pix config get memory_watcher_model    # fact capture
pix config get ollama_bridge_model     # the local model the sandbox exposes
```

Check that the tags those keys name are actually pulled:

```console
$ ollama list
NAME                       ID              SIZE      MODIFIED
qwen3.5:9b                 6488c96fa5fa    6.6 GB    2 weeks ago
nomic-embed-text:latest    0a109f422b47    274 MB    7 weeks ago
```

If a tag is missing, `ollama pull <tag>` it. Semantic recall latches off on the
first embed failure and does not retry, so restart the daemon after pulling:
`pix serve stop && pix serve`.

## 6. Daily use

```bash
cd ~/code/my-project
pix
```

That is the loop. `pix` launches the sandbox for this directory, or reattaches
to an existing one, running or stopped. The last shell to leave a sandbox tears
it down. The final line names the saved session and the `pix resume` command
that reopens it.

| Command | Does |
| --- | --- |
| `pix` | at a terminal, same as `pix run` here; piped or in CI, prints status instead |
| `pix run <dir>` | the explicit launch, safe in a script |
| `pix ls` | your `pix-*` sandboxes: NAME, STATE, DIR |
| `pix rm <name>` | remove one sandbox |
| `pix status` | what is up, what is down, what is next |
| `pix doctor` | the same, with evidence and the exact fix commands |
| `pix resume <session>` | reopen the session named at exit |
| `pix task new <name>` | an isolated clone plus branch plus sandbox, for parallel work |
| `pix reset` | move host config and data aside and remove every `pix-*` sandbox |

Inside a session, `/todos` toggles the task widget without changing tasks.
Use `/todos hide` or `/todos show` for an explicit state, `Alt+T` for the
keyboard shortcut, and `/todos clear` only when you want to delete the list.
Visibility survives `/reload` and session-tree navigation.

`pix reset` is reversible. It renames three directories to timestamped `.bak-`
siblings rather than deleting them, and it leaves your provider keys and your
git repos alone.

Removal is never forced by default. It requires a kernel-verified proof that no
shell still references the sandbox. `pix rm <name> --force` is the one explicit
override.

## 7. Your own instructions and skills (optional, no pack needed)

One directory on your host loads into every session:

```
~/.local/share/pix/context/
  AGENTS.md               # standing instructions
  skills/<name>/SKILL.md  # your own skills
```

It is bind-mounted read-write at the same path inside the sandbox, and it is
created if absent, so a session can author its first skill without going back to
the host. Edits land on your host immediately. Put the whole directory in git
and commit from either side:

```bash
cd ~/.local/share/pix/context && git init && git add . && git commit -m "my setup"
```

The two files have different lifecycles. `skills/` is read live, so `/reload`
picks up an edit in the running session. `AGENTS.md` is read once at launch, so
an edit applies to your next session.

## 8. MCP servers and tool keys (optional)

MCP servers are registered once on the host:

```bash
pix mcp add <name> --url <url>   # a hosted server
pix mcp auth <name>              # OAuth it, if it needs one
pix mcp ls                       # what the gateway knows about
```

Registration is host state. A sandbox picks up everything registered at the
moment it launches, so add first, then start the sandbox. To catch a running one
up: `pix rm <box>`, then `pix run`. A tick in `pix mcp ls` means registered, not
working. `pix doctor` is what checks working.

Pix ships no MCP servers of its own. Anything other than a hosted URL (a local
binary, a container) is declared by a pack, and `pix mcp add <name>` with no URL
registers what the active pack declared.

Tool keys are separate from model keys. They buy a capability rather than a
model, and a missing one degrades that capability instead of blocking a launch:

```bash
pix secret set PARALLEL_API_KEY op://vault/item/field
pix secret sync
pix secret ls
```

Values never touch disk. `op-refs.env` maps an env var to an `op://` reference,
resolved just in time.

## 9. Packs (optional)

A pack is a git repo carrying skills, knowledge, MCP integrations, CLI wrappers
and config. It is the redistribution mechanism: a team or a second machine gets
your whole setup in one command.

```bash
pix pack ls                  # the active pack, if any
pix pack show                # its skills, setup hooks, integrations
pix pack use git+https://github.com/your-org/work-pack.git#ref=main
```

You do not need a pack. Sections 7 and 8 cover a personal machine completely.
Reach for a pack when you are handing your setup to someone else, or when you
want an integration that runs a local binary rather than a hosted URL.

Adopting a pack that runs code on your host halts at a reviewable bill of
materials first, defaulting to No. A non-interactive adoption fails closed
unless you pass `--yes`. `pix setup --pack <url>` does the same thing during
first-time setup. See [docs/design/packs.md](docs/design/packs.md).

## 10. Running the sandbox without the Pix launcher

You can, through the kit:

```bash
sbx run pix --kit "git+https://github.com/mcavage/pix.git#dir=pi-kit"
```

`pix` there is the agent name declared by the kit, not an image reference. The
kit pins the image version. Confirm the reference resolves before you run it:

```console
$ sbx kit inspect "git+https://github.com/mcavage/pix.git#dir=pi-kit"
Name:        pix
Kind:        sandbox
Schema:      v2
Display:     Pix
Binary:      pi
Template:    docker.io/mcavage/pix:0.1.43
AI File:     AGENTS.md
Policies:
  Network:      22 allow, 0 deny
  Credentials:  5 sources
  Environment:  1 variables, 5 proxy-managed
  Commands:     2 install, 0 startup, 0 init files
```

What you give up is everything the launcher does on the host:

| You keep | You lose |
| --- | --- |
| the sandbox and the pi agent | MCP servers registered from your config |
| the kit's network allowlist | memory autostart and recall |
| the pinned image | the active pack's `bin/` wrappers and skills |
| | the trusted host state handed to the agent |

## 11. What actually constrains the agent

`AGENTS.md`, skills, and a pack's context are guidance a model reads. They are
not enforcement. The agent can edit those files, and a model can decline to
follow an instruction. Do not write a rule there and consider a dangerous action
blocked.

The things that hold:

- **The sandbox.** The agent cannot touch your host except through the
  directories you mounted. That is why it needs no permission prompts.
- **The network allowlist.** A domain absent from the kit's
  `permissions.network.allow` is unreachable from inside. Credentials never
  enter the sandbox; the host proxy swaps a sentinel for the real key on the way
  out.
- **A `tool_call` gate, if you write one.** A pi extension that hooks
  `tool_call` and returns `{block: true, reason}` refuses an action before it
  runs. Pix ships no such extension. An extension is a single `.ts` file in
  `~/.pi/agent/extensions`; pi's `docs/extensions.md` has `permission-gate.ts`
  and `protected-paths.ts` examples.

Local and container MCP servers are a second thing to know: they run on the
host, outside the sandbox, with your host-user privileges, and content they
return can be included in the conversation sent to your model provider. See
[SECURITY.md](SECURITY.md).

## 12. Working on Pix itself

Maintenance is `make`, not the CLI:

```bash
make gate      # the fast test gate
make build     # build the image (needs a DHI-entitled Docker account)
make load      # build and load it into the sandbox image store
```

## Where to go next

- [docs/getting-started.md](docs/getting-started.md): a first session, end to end.
- [docs/reference.md](docs/reference.md): the full command reference, one section per verb.
- [docs/memory.md](docs/memory.md): how memory captures, ranks, and backs up.
- [AGENTS.md](AGENTS.md): the architecture, if you are extending Pix.

## License

MIT. See [LICENSE](LICENSE).
