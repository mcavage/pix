# Pix

An AI workspace for coding, research, and everyday work. Pix runs the
[Pi agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) in a
Docker Sandbox with your chosen models, reusable skills, and optional account
connections.

Open a terminal in the folder you want to work on, run `pix`, and describe what
you need. The agent can read and edit that folder, run tools, and delegate work
to other models. Your files and saved conversations remain after you exit.

## Get started

Pix's supported host setup is macOS with [Homebrew](https://brew.sh),
[Docker Desktop](https://docs.docker.com/get-docker/), and Git. Start Docker
Desktop, then install and sign into Docker Sandboxes:

```bash
brew install docker/tap/sbx
sbx login
```

<!-- PIX_PRIMARY_PATH_START -->
```bash
brew install mcavage/tap/pix
pix setup
```
<!-- PIX_PRIMARY_PATH_END -->

Setup guides you through choosing a model and connecting what it needs. You can
rerun it if interrupted; completed setup is kept.

- **API models:** one OpenAI, Anthropic, or Google API key is enough. Store it in
  1Password, install its CLI with `brew install 1password-cli`, and enable the
  desktop app's CLI integration. Paste the field's `op://...` reference when
  prompted, rather than the key itself.
- **Ollama:** if Ollama is running, setup offers supported installed models,
  including available cloud models. A local model needs no provider API key;
  Ollama Cloud uses your Ollama account.
- **An environment supplied by your team:** follow its setup instructions or
  [add it below](#use-an-environment). It may provide models without personal API
  keys.

Start a session in an existing project or a new folder:

```bash
mkdir -p ~/pix-work
cd ~/pix-work
pix
```

Try a request such as “Summarize the documents in this folder” or “Review this
project and suggest the smallest useful improvement.” Inside the session,
`/getting-started` gives you a tour.

## Use an environment

An environment supplies a set of models, skills, and connections. `default` is
created for you; other names are entirely your choice.

For an environment you already have on disk:

```bash
pix env add ~/path/to/environment team
pix setup --env team
pix run --env team
```

`pix env add` also accepts a Git repository URL. Setup asks whether to make the
new environment your default. Answer **yes** to use it whenever you type `pix`.
You can change that choice later:

```bash
pix env default team
pix env list
```

Environment setup may open your browser to connect accounts. If it needs to run
tools on your computer, Pix asks for your consent. Changes to that access can
require approval again.

## Daily use

| What you want | Command |
| --- | --- |
| Start in the current folder | `pix` |
| Start in another folder | `pix run ~/path/to/project` |
| Use an environment for this session | `pix run --env team` |
| Use a particular configured model | `pix run --model openai/gpt-6-astra` |
| Keep a sandbox's installed tools between sessions | `pix run --keep` |
| List your sandboxes | `pix ls` |
| Remove an idle sandbox | `pix rm NAME` |
| Work on a separate Git task checkout | `pix task new feature-name` |
| See all commands | `pix help --all` |

Ordinary sandboxes are removed when their last session exits. Your mounted
project files and `.pi-sessions` conversations are kept; tools or files stored
only inside the sandbox are discarded. Use `--keep` when you need those too.
In scripts, use the explicit `pix run` command; bare `pix` requires a terminal.

## Remember useful context

Inside a session:

```text
/remember Use concise summaries with the recommendation first.
/recall writing preferences
/forget <memory-id>
```

Memory persists across sessions. Capture is explicit by default. When Ollama's
embedding model is available, recall can search by meaning as well as keywords;
otherwise keyword recall still works. Setup can offer to prepare that embedding
model. See [memory](docs/memory.md) for capture settings, scopes, and backups.

## Push code to GitHub

GitHub access is optional. To let the agent push branches and open pull requests,
store a suitable token in 1Password and give Pix its reference:

```bash
pix secret set GITHUB_TOKEN op://vault/item/field
```

Replace the example with your token's actual reference. The next launch supplies
it to that sandbox. The sandbox already includes `git` and `gh`; installing or
logging into `gh` on your host does not configure Pix's sandbox credentials.

## When something needs attention

```bash
pix doctor
```

Doctor checks readiness and suggests fixes without changing your setup. Rerun
`pix setup` to finish setup, or `pix setup --env team` to reconnect that
environment's accounts. Add `--verbose` to setup, run, or doctor for diagnostics.
If the problem is Docker Sandboxes itself, start with `sbx diagnose`.

Upgrade with `brew upgrade mcavage/tap/pix`. Your next launch updates Pix's own
runtime and memory service as needed. Connection changes remain part of setup.

## Your files and accounts

The agent can change files in mounted folders and use whatever account access
you authorize. Sandboxing does not undo edits or prevent an authorized tool
from acting on an external service. Review changes before committing or sharing
them. Connected content and recalled memory may be sent to your selected model
provider; see [the security boundary](SECURITY.md).

Pix keeps its settings, environment definitions, credential references, and
memory under `~/.pix`. Set `PIX_HOME` to use a separate installation; cleanup is
scoped to that home. Provider keys stay in 1Password, and Pix supplies them through
sandbox-scoped credentials rather than host-global sbx secrets.

## More

- [First-session guide](docs/getting-started.md)
- [Command reference](docs/reference.md)
- [Memory](docs/memory.md)
- [Documentation index](docs/README.md)
- [Contributing](CONTRIBUTING.md) and [maintainer instructions](AGENTS.md)

MIT. See [LICENSE](LICENSE).
