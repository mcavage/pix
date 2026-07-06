# sbx 0.34: custom standalone kits get no host-credential injection

**Component:** `sbx` (Docker Sandboxes) · **Version:** `v0.34.0` (`2eae0c4fc3894475da3318615f69783b0e7be747`, installed via Homebrew) · **Type:** regression / gap · **Also present on `main`** as of this writing.

## TL;DR

A custom **standalone** kit (`kind: sandbox` with its own agent name and image) receives **zero** host-credential injection. Every declared provider credential resolves to `SBX_CRED_<SERVICE>_MODE=none`, the `proxy-managed` sentinel env vars are never set, and the in-VM agent sees no configured providers. The credential's format in the kit (v1 `serviceAuth`/`serviceDomains` or v2 `credentials[].apiKey.inject`) is irrelevant, injection is gated earlier, on the **agent name**, not on the kit's declared credentials.

The gate recognizes only built-in agent names, and built-in agent names cannot be used by a custom kit. Those two facts are mutually exclusive, so there is **no kit configuration that makes injection work** for a standalone custom agent on 0.34. Downgrading to 0.33 restores it.

## Symptom

A working multi-provider kit (agent `pi-stack`, runs `pi` with Anthropic/OpenAI/Google models) that worked on sbx 0.33 breaks immediately after upgrading to 0.34:

```
Warning: No models match pattern "anthropic/claude-opus-4-8"
Warning: No models match pattern "openai/gpt-5.5"
Warning: No models match pattern "google/gemini-3.1-pro-preview"
Model scope: gemma4:latest    # only the local Ollama model, registered by an in-VM extension, survives
```

Inside the VM:

```
ANTHROPIC_API_KEY=[]            # sentinel never set
OPENAI_API_KEY=[]
GEMINI_API_KEY=[]
SBX_CRED_ANTHROPIC_MODE=none
SBX_CRED_OPENAI_MODE=none
SBX_CRED_GOOGLE_MODE=none
SBX_CRED_GITHUB_MODE=none
```

The provider secrets are present in the global store (`sbx secret ls` shows `anthropic`, `openai`, `google`, `github`), and a binding exists in `~/.config/sbx/credentials.yaml`. Neither matters, see the root cause.

## Root cause

The `SBX_CRED_<SERVICE>_MODE` env vars are derived from a single aggregate boolean, `hasHostCredentials`:

`sandboxlib/sandbox/credential_mode.go`
```go
func credentialModeEnv(svcs []agent.CredentialServiceRef, useOAuth, hasHostCredentials bool) map[string]string {
    for _, s := range svcs {
        mode := CredModeNone
        switch {
        case s.HasOAuth && useOAuth: mode = CredModeOAuth
        case hasHostCredentials:     mode = CredModeAPIKey
        }
        env[fmt.Sprintf("SBX_CRED_%s_MODE", strings.ToUpper(s.Service))] = string(mode)
    }
}
```

`hasHostCredentials` comes from the daemon at create time:

`sandboxd/pkg/server/backend_dockernext.go` (~line 1486)
```go
hasHostCreds := sandbox.HasAgentCredentials(info.Spec.AgentName,
    info.Spec.Credentials.Sources, info.Spec.Credentials.Values)
```

`HasAgentCredentials` delegates to `hasAgentCredentials`, which resolves credentials **by agent name**:

`sandboxlib/sandbox/sandbox.go` (~line 444)
```go
func hasAgentCredentials(agentName string, sources map[string]...CredentialSource, values []...CredentialValue) bool {
    agents := loadEmbeddedAgents()
    if agents != nil {
        if pluginAgent, ok := agents[agentName]; ok {
            // built-in agent with a ServiceDetectionProvider: check sources/values
            // against the agent's declared services...
            return ...
        }
    }
    // Fallback: static map for non-embedded agents.
    agentToService := map[string]string{
        "claude": "anthropic", "codex": "openai", "opencode": "openai",
        "gemini": "google",    "copilot": "github",
    }
    serviceName, ok := agentToService[agentName]
    if !ok {
        return false                 // <-- any custom agent name lands here
    }
    ...
}
```

For a custom agent (`pi-stack`): it is not in `loadEmbeddedAgents()`, and not a key in the static map, so the function `return false`s **before ever consulting `sources` or `values`** — even though the kit's declared credentials (and the resolved values) are passed in as arguments and are fully populated. `hasHostCredentials=false` then forces every service to `MODE=none` in `credentialModeEnv`, and `vm_configurator.go` (~line 212, `if !v.hasHostCredentials`) skips the proxy-managed auth wiring, so the sentinel env vars in `sandboxlib/kit/agent.go` are never set.

Note the aggregate shape: `hasHostCredentials` is one boolean for the whole sandbox (there is a `TODO(multi-service)` in `credential_mode.go` acknowledging it is not yet per-service). So if the agent name mapped to *any* one present service, *all* declared services would flip to `APIKey`.

## Confirmation matrix (empirical, this binary)

Detached `sbx run … --detached` on `v0.34.0` (`2eae0c4f`), then reading
`SBX_CRED_<SERVICE>_MODE` and the sentinel env var inside the VM. Provider secrets
present in the global store (`sbx secret set -g`):

| Agent | kind / schema | credential source | `SBX_CRED_*_MODE` | sentinel set |
|---|---|---|---|---|
| built-in `gemini` | (embedded) | global store | **`apikey`** | **yes** |
| custom `pi-stack` | `sandbox` / v2 | global store | `none` | no |
| custom `pi-stack` | `agent` / v1 | global store | `none` | no |
| **contrib `pi`** (Docker's reference) | `agent` / v1 | global store | `none` | no |
| contrib `pi` | `agent` / v1 | host env var | `none` | no |

The built-in agent is the **positive control**: same store, same host, injection
works. Every custom kit, including the reference kit in `docker/sbx-kits-contrib`,
gets `none`. So this is not a kit-authoring mistake (`kind`, schema version,
`serviceAuth` vs `apiKey`, store vs env all make no difference); it is the
name-gated detection in the daemon. The "use v1 / copy the contrib kits" guidance
does not resolve it on this binary, because the contrib kits hit the same gate.

## Why there is no kit-side workaround

The one lever the code leaves open, make the agent name one the gate recognizes, is closed by a separate guard:

```
$ sbx run codex --kit ./my-kit .
ERROR: agent "codex" is already registered (built-in agents cannot be overridden by a kit)
```

So:
- Credential injection requires a **built-in** agent name (`claude`/`codex`/`opencode`/`gemini`/`copilot`) or an embedded `ServiceDetectionProvider`.
- A custom kit **cannot** use a built-in agent name.

These are mutually exclusive. There is no kit spec (v1 or v2), no `credentials`/`serviceAuth`/`apiKey` declaration, and no secret-store or binding state that makes a standalone custom-agent kit inject. The only shipping path that injects is a **mixin** kit stacked on a built-in agent (`sbx run claude --kit ./mixin`), which forces the built-in agent's image and single provider.

## Impact

Any standalone `kind: sandbox` kit that defines its own agent and needs provider API keys is non-functional for credentials on 0.34. This includes any multi-provider agent (an agent that talks to more than one of Anthropic/OpenAI/Google), since no single built-in agent name covers them and custom names are rejected. On 0.33 these kits worked.

## Suggested fix

`hasAgentCredentials` already receives the kit's declared `sources` and the resolved `values`. For an agent that is neither embedded nor in the static map, instead of `return false`, fall back to those arguments:

```go
// after the embedded + static-map paths, for an unrecognized (custom) agent:
for range sources { return true }              // kit declares at least one credential source
for _, v := range values { _ = v; return true} // or a value resolved
return false
```

or, better, make the check per-service and feed the result into a per-service `hasHostCredentials` (the `TODO(multi-service)` already anticipates dropping the single aggregate). The information needed is present at the call site; the gate simply discards it for custom agents.

## Reproduction

1. Author a `kind: sandbox` kit with a novel agent name (e.g. `name: my-agent`), its own image, and a declared credential (v1 `network.serviceAuth`/`serviceDomains` + `credentials.sources` + `environment.proxyManaged`, or v2 `credentials[].apiKey`).
2. `sbx secret set -g anthropic` (and/or openai/google).
3. `sbx run my-agent --kit ./my-kit .`
4. Inside the VM: `printenv ANTHROPIC_API_KEY` is empty; `env | grep SBX_CRED` shows `..._MODE=none`; the agent reports no configured providers.
5. Confirm the gate: `sbx run codex --kit ./my-kit .` → `agent "codex" is already registered (built-in agents cannot be overridden by a kit)`.

## Workarounds today

- **Downgrade to sbx 0.33**, where standalone custom kits inject.
- **Mixin onto a built-in agent** if the single-provider, built-in-image constraints are acceptable.
- For local-only models (e.g. Ollama), register the provider from an in-VM extension so it bypasses host-credential injection entirely (this is why the local model kept working in the symptom above).
