# Change brief — read this before touching tests

Temporary working document for the integrations remediation. Delete when the
work lands. Full context: `docs/design/integrations-remediation.md`.

## What changed, in one paragraph

Pix used to have a hardcoded Google Workspace (`gog`) integration and a
"local MCP bridge" (`pix-host mcp <name>`) whose list of servers had been
permanently EMPTY since the last built-in server was externalized. Both are
gone. **Every MCP server now comes from the active pack's manifest**, in one of
four transports, and pix special-cases no vendor.

## The new model

```toml
[[integrations]]
  name  = "Google Workspace"
  mcp   = "google-workspace"

  # exactly ONE transport:
  command = "gog"                     # host binary over stdio   (NEW)
  # image = "bamboohr-mcp:0.0.1"      # container the gateway runs
  # manifest = "..."                  # OCI server manifest
  # url   = "https://..."             # remote endpoint, gateway OAuths it

  args       = ["--readonly", "mcp"]  # LITERAL argv, never templated
  env        = "GOG_KEYRING_PASSWORD" # the op:// secret
  env_keys   = ["GOG_ACCOUNT"]        # extra env NAMES forwarded
  env_values = { X = "y" }            # non-secret literals
  probe      = ["gog", "auth", "doctor"]  # health probe          (NEW)
  setup      = "google-workspace"
```

### Symbols deleted (do not reintroduce)

| deleted | replacement |
|---|---|
| `config.GWServerName`, `config.GWInstallCmd` | nothing — no vendor is named in core |
| `config.Config.GogAccount`, `SetGogAccount` | pack `env_keys` (e.g. `GOG_ACCOUNT` in op-refs.env) |
| `google_workspace_account` config key | same |
| `config.NonSecretOpRefsKeys` (global map) | `secret.NonSecret` map, supplied by the caller from the pack |
| `mcp.GogHardenedArgv` | pack-declared `args` |
| `mcp.LocalMCPNames` | pack declaration lookup |
| `mcp.McpRegistrar.{Gog,Account,HostBin,GogUseOp}` | `.servers` (pack specs) + `.resolved` (PATH lookups) |
| `mcp.Credentials.GogKeyring` | nothing — op-run wrapping is decided by whether a server declares `EnvKeys` |
| `pack.LocalMCPClassifier`, `pack.PackLocalMCP` | nothing — classification is a map lookup |
| `pack.AcceptedGoPluginServicesForSelf` | `pack.AcceptedGoPluginServices(p)` |
| `packinfo.Manifest.GogAccount` (`gog_account`) | nothing |
| `hostStateGog` / `HostState.Gog` | nothing |

### Signatures changed

```go
config.MCPContainer                    -> config.MCPServer  (+ Command, Args, Probe; + HostExec())
packinfo.ContainerMCP(p)               -> packinfo.ServerMCP(p)
packinfo.ActiveContainerMCP(cfg)       -> packinfo.ActiveServerMCP(cfg)
packinfo.NonSecretEnvNames(p)          -- NEW
packinfo.ActiveNonSecretEnvNames(cfg)  -- NEW

secret.ParseOpRefs(content)            -> ParseOpRefs(content, nonSecret NonSecret)
secret.RunSecretLs(env, out)           -> RunSecretLs(env, out, nonSecret)
secret.RunSecretSet(env,out,k,v)       -> RunSecretSet(env, out, k, v, nonSecret)
secret.RunSecretSetLocked(...)         -> same + nonSecret
   (pass nil where the allowlist is irrelevant — nil means "must be an op:// ref")

mcp.RegisterServers(cfg, env, out, requested, hostResolver, containers, creds)
   ->              (cfg, env, out, requested, servers, creds)

pack.ComputeHostBoM(p, gogAccount, isLocalMCP) -> ComputeHostBoM(p)   // PURE
pack.VerifyPackLaunchTrust(p, gogAccount, env) -> (p, env)
pack.RegisterFn: drops the hostResolver param
doctor.MCPServers(cfg, env, hostResolver)      -> MCPServers(cfg)
provision.ValidateSetupSemantics(opts, env, r) -> (opts)
provision.validateOnboarding(r, env, r)        -> (r, declared map[string]config.MCPServer)
provision.validateOnboardingShape(r)           -- NEW (shape only, pre-adoption)
```

### New behaviour worth testing

1. **op-run wrapping is by declaration.** `ExecArgv` wraps a server in
   `op run --env-file` **iff** op-refs exists AND the server declares
   `EnvKeys`. A credential-free server is never wrapped — it must not share
   fate with unrelated refs in the file.
2. **Unknown server names are an error, not a skip.** `RegisterServers`
   returns a non-nil error naming any requested server no active pack declares.
3. **Command resolution is a hard failure.** Registering a `command` server
   whose binary is not on PATH returns an error naming the server, the binary
   and the pack's install hint. It must NOT register a broken command.
4. **Doctor distinguishes registered from working**, via `health.MCPServer`:
   - `Undeclared: true` → gap, even when the gateway lists it (this is the
     "registration outlived its pack" case)
   - `Command` set but not on PATH → gap
   - `Probe` declared → run it; non-zero exit is a gap
   - `Probe` empty → note says working order is **unverified**, NOT healthy
5. **Declarative pack setup** (`packinfo.SetupStep.Require` / `.Apply`):
   - require kinds: `bin` (needs `name` + `install`), `op-ref` (needs `env`),
     `probe` (needs `argv`); apply kinds: `interactive`, `exec`
   - unknown kinds are refused at LOAD
   - a step with `require` but no `apply` reports what is missing and fails,
     rather than claiming success
6. **Integrations must declare a transport.** An `[[integrations]]` entry with
   an `mcp` name but no command/image/manifest/url is refused at load.

## Fingerprint compatibility — READ THIS

`workflow/pack/trust.go` computes the Tier-1 consent fingerprint. Its `fpDoc`
JSON encoding is **load-bearing**: changing field names, order, or omitempty
re-gates every already-accepted pack and forces every user through a trust
re-prompt.

The new `Require`/`Apply` fields on `fpSetup` are `omitempty` **on purpose**, so
a pack using the executable form encodes byte-identically to before. Preserve
that. If you write a test here, assert the compatibility (an old-shape pack's
fingerprint is unchanged), never just the new shape.

## Rules for this work

- **Update tests to the new architecture. Do not delete a test to make it
  pass.** If an assertion no longer has a subject (it tested gog specifically),
  rewrite it against the generic mechanism that replaced it, so the coverage
  survives the refactor.
- If a test documents behaviour that is now genuinely gone, delete it and SAY
  SO in your report, with the reason.
- If you find a real BUG in the new production code, report it. Do not paper
  over it by weakening an assertion — that is the single most damaging thing
  you could do here.
- Do not reformat or restructure code you are not fixing.
- `gofmt` everything you touch. Do not commit.
