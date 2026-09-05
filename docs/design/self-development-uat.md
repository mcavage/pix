# Self-development UAT

> **HISTORICAL — pre-v2 design note.** This document predates the accepted
> Pix v2 surface and architecture (`docs/design/pix-v2-surface.md`,
> `docs/design/pix-v2-architecture.md`), which supersede it. Commands,
> files, and components described here may no longer exist. Nothing in it
> is a description of current behavior; read it as history only.


Status: PROPOSED

## Problem

A pix agent can edit and test pix inside its sandbox, but it cannot run the part
that matters most: a real host build followed by real `pix` and `sbx` lifecycle
operations. Docker, the sbx image store, OAuth callbacks, and the host browser
all live outside the sandbox.

The current release check, `scripts/host-uat.sh` (the deleted `scripts/macos/verify-pix-lifecycle.sh` is gone), therefore
requires a person to build, load, run, observe, and clean up the host-side test.
That breaks the autonomous development loop.

Pix needs a constrained host capability that lets an agent:

1. submit a committed candidate;
2. build host binaries and the sandbox image;
3. create dedicated disposable sandboxes;
4. run lifecycle and integration scenarios;
5. complete MCP OAuth through a host browser;
6. inspect browser links, logs, structured results, and screenshots;
7. clean up and iterate without a person driving the run.

It must not expose a host shell, normal pix sandboxes, the production image tag,
or the user's normal browser profile.

## Decision

`pix run --dev` becomes the self-development mode. It already means “use this
local pix checkout's kit and load its skills live.” A newly created `--dev`
session will also receive an ephemeral, session-scoped UAT MCP server. There is
no second `--uat` flag.

`make run` must use the same launcher path rather than invoking `sbx run`
directly, so both entry points get identical dev behavior:

```text
make run -> out/pix run --dev
```

Attaching to an existing sandbox does not retrofit the capability. Like kit and
static MCP changes today, the sandbox must be recreated. An explicit `--dev`
attach therefore fails closed when the sandbox has no recorded session UAT
registration and prints the exact `pix rm <box> && pix run --dev` recovery. It
must never attach successfully while omitting `uat_capabilities`. At a terminal,
`pix --dev` is the direct shorthand for `pix run --dev`; like every implicit
launch it refuses non-interactive use, where the explicit spelling is required.

## User experience

The development loop is:

```text
edit -> local tests -> commit -> submit UAT scenario
     -> host builds candidate -> disposable UAT run
     -> inspect events/artifacts -> fix -> repeat
```

A run starts from a commit, never uncommitted bytes or an arbitrary host path:

```json
{
  "commit": "9f2c1ab",
  "scenario_path": "uat/scenarios/oauth-smoke.yaml"
}
```

The host verifies that the commit is reachable from the checkout bound to the
UAT session, then makes a disposable local clone for the run.

## Session-scoped MCP: gateway relay + host-context worker

The first implementation planned to have the sbx gateway spawn `pix-host
uat-mcp` directly, with that one process both speaking stdio MCP to the client
and constructing the UAT Runner (which shells out to `git`, `docker`, `sbx`,
and a host browser). Building it exposed why that shape cannot work: sbx's
local gateway spawns MCP host commands from its OWN daemon process tree, not
from the operator's interactive shell. A process born under that ancestry
never inherits the operator's `sbx`/Docker login session, SSH agent, or
browser-profile state — the exact host authentication the UAT Runner needs to
drive real lifecycle operations. Every gateway-spawned `uat-mcp` therefore hit
"not authenticated to Docker"-shaped failures that had nothing to do with the
scenario under test.

The fix is a two-process split, authorized as the explicit fallback in this
document's original MVP note ("use a per-session loopback worker … the tool
contract and runner remain the same") rather than an exec broker or a
network-exposed backdoor:

- **`pix-host uat-mcp`** stays what the gateway spawns per client connection,
  but it is now a DUMB stdio↔Unix-socket relay. It takes one flag,
  `--connect <socket>`, dials the session's worker socket (bounded retries,
  then an actionable error naming `pix run --dev`), and copies bytes
  bidirectionally until either side closes. It constructs no Runner, and it
  must never import `os/exec` or `pix/host/workflow/uat` again — a structural
  sentinel test (`TestUatMcpGatewayIsADumbRelay`) pins that fact at the source
  level so the regression cannot silently return.
- **`pix-host uat-worker`** owns the real UAT Runner and every host command it
  runs. It is started later, by `pix run --dev` itself — a process launched
  from the operator's own interactive shell, which is exactly where the
  authenticated `sbx`/Docker/browser context lives. It listens on a
  session-owned Unix socket under the same 0700 per-session state directory
  `RegisterMCP` already creates (`<state>/sessions/<sessionID>/uat.sock`),
  accepts one client connection at a time (single-flight), and survives a
  client EOF: only that connection's MCP `Serve` loop ends, never the worker
  process, so a gateway relay reconnecting (a new pi session in the same
  sandbox) picks the same live Runner back up.

The socket path is hardened with the same session-id/root rules
`RegisterMCP`/`removeSessionState` already enforce (absolute, real directory,
never a symlink, 0700, owned by the current uid), plus a symlink check on the
socket file itself, before either side (the worker's `Listen` or the
gateway's `Dial`) touches it.

The registration argv changes accordingly: `sbx mcp add <name> --command
<pix-host> --args uat-mcp --args --connect --args <socket>`, with the repo
path, state root, and session id no longer part of the gateway command at all
(the worker receives those directly from `pix run --dev`, not from the
gateway's registration). The tool contract — `uat_capabilities`, `uat_submit`,
`uat_status`, `uat_artifact`, `uat_browser_action`, `uat_abort` — and the
underlying Runner are unchanged; only the transport and which process
constructs the Runner moved.

Wiring `pix run --dev` to actually spawn and supervise `uat-worker` (start
ordering, lifecycle, teardown alongside the sandbox) is the next unit, not
this one: this change lands the reusable transport primitives — the socket
hardening, the relay, the worker's accept loop, and the planner/registration
argv — with no `run_cmd` lifecycle wiring yet.

Normal pix sandboxes never receive the UAT server. A startup reaper removes
expired UAT registrations and resources after a hard crash.

## Security boundary: operator environment inheritance

`uat-worker` executes submitted candidate code on the host as a child process
of the operator's own `pix run --dev` invocation, precisely because that is
the only process in the tree holding the operator's authenticated `sbx`,
Docker, and browser-profile session (see above). That inheritance is not
incidental — it is the fix that makes `uat-worker` work at all — and it comes
with a consequence that must be stated plainly, not assumed away.

`uat-worker` inherits the operator shell's full environment, unfiltered. It
does not scrub, allowlist, or redact environment variables before building
candidate binaries or running scenario steps. Any environment variable
exported in the shell that ran `pix run --dev` — including a long-lived
provider or cloud API key the operator happens to have exported for
unrelated work — is visible to whatever the submitted candidate build and
scenario execute on the host. This is an accepted, dev-only boundary, not a
gap slated for a later unit: `--dev` is explicitly the mode where a
committed but not-yet-trusted candidate gets host execution, and proving that
candidate's behavior before it ships is the point of this design. No
mechanism described here removes, wraps, or filters the inherited
environment, and none should be assumed present until one is designed,
implemented, and covered by a test that proves scrubbing actually happens.

**Operators must not launch `pix run --dev` from a shell that has exported
long-lived provider or cloud API secrets.** Use a clean shell, or one scoped
to only the credentials `sbx`, Docker, and the browser controller actually
need, before running `--dev`. Treat any secret exported into that shell as
potentially exposed to whatever a submitted UAT scenario executes.

This boundary is dev-only by construction. `uat-mcp`/`uat-worker` are wired
only into sessions started with `--dev` (see "Decision" above: normal pix
sandboxes never receive the UAT server). A production or non-dev session
never spawns a worker, never receives `uat_capabilities`, and never executes
submitted candidate code on the host. The risk above is scoped to the
self-development loop, not to ordinary `pix run` usage.

## Tool contract

The server exposes a closed tool set:

### `uat_capabilities`

Returns host prerequisites, browser-profile state, budgets, scenario schema,
named checks, and legal actions.

### `uat_submit`

Validates a committed scenario and queues a run. `dry_run` returns the fully
resolved plan without building or mutating anything.

### `uat_status`

Long-polls a run using an event cursor. Events are append-only and
sequence-numbered so compaction or a dropped response cannot lose state.

### `uat_artifact`

Returns bounded log tails, JSON results, DOM snapshots, or screenshots. The
host performs size checks and credential-shaped redaction first.

### `uat_browser_action`

Acts on the active allowlisted page. The initial vocabulary is `snapshot`,
`click`, `wait_for_url`, and `read_visible_text`. It accepts element references
from a prior snapshot, not arbitrary JavaScript or arbitrary navigation URLs.

### `uat_abort`

Cancels the run, kills owned process groups, removes leased sandboxes and MCP
registrations, closes the browser, and verifies cleanup.

There is no shell tool, command tool, environment passthrough, patch upload, or
host filesystem browser.

## Scenario format

Scenarios are committed YAML with a closed action and assertion vocabulary:

```yaml
schema: pix.uat/1
name: mcp-oauth-smoke
timeout: 20m
needs: [docker, sbx, browser]

steps:
  - id: build
    do: candidate.build

  - id: register
    do: mcp.register
    with:
      fixture: notion
      unique_name: true

  - id: authorize
    do: mcp.authorize
    with:
      registration: register
    expect:
      browser_opened: true
      callback_completed: true
      auth_status: authorized

  - id: launch
    do: sandbox.create
    with:
      fixture: empty-repo
      mcp_from: register

  - id: tools
    do: sandbox.probe
    with:
      probe: mcp_tools_present

  - id: link
    do: browser.check
    with:
      source: sandbox
    expect:
      scheme: https
      status: 200
      screenshot: capture
```

Legal actions are implemented by Go code. Unknown actions fail validation.
Assertions support exit codes, bounded output matching, JSON paths, presence,
absence, HTTP status, URL transitions, and screenshot capture. There is no
expression evaluator.

The existing host UAT checks should acquire stable IDs, such as:

```text
lifecycle.attach_fingerprint
lifecycle.multi_shell_teardown
lifecycle.keep_and_orphan_reaper
services.memory_restart
services.managed_restart
mcp.remote_oauth
browser.link_opened
```

The shell release check should eventually call the same runner. Pix must not
maintain two independent implementations of the same host assertions.

## Candidate build isolation

The runner never executes the candidate's Makefile on the host.

It owns a fixed build recipe:

1. clone the submitted commit into the run directory;
2. build Darwin `pix` and `pix-host` binaries inside a controlled builder
   container with `CGO_ENABLED=0`;
3. build the sandbox image with BuildKit;
4. tag it only inside a reserved namespace such as
   `docker.io/mcavage/pix:uat-<commit>-<run>`;
5. load that exact tag into sbx;
6. generate a run-specific kit pinned to the resulting digest.

The planner refuses `latest`, the production pix tag, tags outside the UAT
namespace, candidate-supplied build commands, and candidate-supplied Docker or
sbx flags.

The UAT kit omits production model credentials by default. A scenario that
needs model access must request an explicit capped test credential.

## Host-state isolation

Each run owns a directory under the state root:

```text
~/.local/state/pix/uat/<run-id>/
  source/
  home/
  config/
  state/
  artifacts/
  events.jsonl
```

Candidate host binaries run with isolated pix config and state. They receive
only the minimum Docker and sbx authentication channels required to exercise
the product. A discovery spike must identify those channels before the runner
is implemented; copying the user's whole home directory is not acceptable.

MCP OAuth tests use unique registration names and remove only registrations the
run created. They never mutate or delete an existing same-name registration.

## Browser and OAuth

Pix uses a persistent, dedicated host browser profile:

```text
~/.local/state/pix/uat-browser/
```

One-time bootstrap opens the profile so the user can log into required identity
providers:

```text
pix uat browser bootstrap
```

Normal UAT runs are hands-off. `pix-host` launches host Chrome with this profile
and controls it through Chrome DevTools Protocol from Go. The profile never
enters a sandbox or artifact bundle.

MFA cannot be automated generically. If the dedicated profile expires or an
identity provider demands MFA, the run is `incomplete`, never passed.

Authenticated and uncredentialed browsing are separate:

- OAuth steps use the dedicated profile.
- Ordinary link checks use a fresh browser context with no stored sessions.

The runner records every requested URL but navigates the authenticated browser
only to exact allowlisted OAuth origins and leased loopback callback ports.
It refuses `file:`, browser-internal schemes, private-network destinations,
unknown origins, and unleased localhost ports.

The first browser spike must determine whether `sbx mcp auth` honors a controlled
`BROWSER` executable. If it does, that executable captures the authorization URL
and hands it to the browser controller. If not, the implementation must find a
machine-readable sbx seam; parsing incidental terminal prose is not a release
quality contract.

## Resource ownership and cleanup

Every mutation belongs to a host-side run lease:

- `pix-uat-*` sandboxes;
- `pix-uat-*` transient MCP registrations;
- UAT image tags and tar streams;
- callback ports;
- browser processes and contexts;
- disposable clones and artifacts.

The UAT namespace validator is stricter than pix's normal `pix-*` validator. A
UAT request can never name or remove `pix-pix`, a task sandbox, or another
normal pix sandbox.

Host-enforced limits:

- one image build at a time;
- no more than three UAT sandboxes;
- run wall-clock and idle deadlines;
- bounded command output and artifact size;
- a fixed autonomous run budget;
- startup cleanup for expired leases.

A run reports `pass`, `fail`, or `incomplete`. Any skipped or unverifiable
required check makes the run incomplete. Success words require a post-mutation
probe: an image is loaded only after sbx confirms it, OAuth is authorized only
after the named status probe confirms it, and a resource is removed only after
it is absent.

## Delivery plan

### Spike 1: prove the boundaries

Prove, without building the scenario engine:

1. transient MCP registration attaches only to its intended dev sandbox;
2. the server can own an asynchronous job and return text and image artifacts;
3. candidate pix can use sbx while pix config and state remain isolated;
4. `sbx mcp auth` exposes a controllable browser-open seam;
5. host Chrome can complete the loopback OAuth callback from the dedicated
   profile.

Any failed assumption changes the transport or isolation design before a public
tool exists.

### MVP

- automatic UAT server for newly created `--dev` sessions;
- capability, submit, status, artifact, browser-action, and abort tools;
- fixed candidate build and UAT image namespace;
- persistent browser profile and OAuth URL capture;
- smoke, lifecycle, browser-link, and one remote MCP OAuth scenario;
- leases, deadlines, quotas, event log, and startup reaper.

### Follow-up

- port the remaining host lifecycle script checks into stable scenario checks;
- make the shell release gate a client of the runner;
- add richer browser assertions;
- add interactive TUI driving only after the non-TUI loop is reliable.

## Explicit non-goals

- arbitrary host commands;
- testing against the user's installed pix binary or production image tag;
- access to ordinary pix sandboxes;
- generic browser automation outside an active UAT run;
- bypassing MFA;
- interactive TUI automation in the first version;
- changing the user's normal pix configuration or browser profile;
- scrubbing, filtering, or sandboxing the operator shell's environment before
  candidate execution (none exists today; see "Security boundary: operator
  environment inheritance").
