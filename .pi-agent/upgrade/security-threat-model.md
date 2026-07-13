# pi-stack redesign — security threat model

Scope: the proposed redesign of pi-stack's HOST surface — (1) go-plugin
out-of-process plugins for all host services incl. credential proxies,
(2) a `curl|sh`-style installer fetching a prebuilt `pi-stack-host`,
(3) `pi-stack setup`, (4) publishable third-party mixin kits, (5) an OKF
knowledge bundle as a private mixin kit.

Reviewer: security-lead. Method: STRIDE per changed surface + supply-chain +
credential/authz review. Findings cite files in this repo.

## Assets and trust boundaries (current)

- **Highest value: long-lived credentials that never enter the VM.** Google
  OAuth refresh token via `gws auth export --unmasked` (`gwstoken.go:52`,
  minted to a short-lived bearer at `gwstoken.go:76-133`); Slack user token
  (`slack.go:24-31`); provider API keys + GH token injected by the sbx proxy
  (`pi-kit/spec.yaml` `credentials`/`proxyManaged`); MCP creds resolved by
  `op run` from `config/op-refs.env` at gateway spawn (Makefile
  `mcp-register`).
- **The host is the trusted zone. The VM is semi-trusted** — it runs a
  full-auto agent with no permission prompts (`spec.yaml` agentContext
  "Posture", AGENTS.md). Prompt injection or a hostile skill/kit can make the
  agent issue arbitrary host-directed requests.
- **The boundary that holds today**: host services bind loopback
  (`memory.go:9-12`, `gwstoken.go` `GWS_TOKEN_BIND=127.0.0.1`,
  `serve.go:31-33`) and are reached only via `host.docker.internal` from the
  allowlisted VM. Brokers hand out *minted tokens*, never raw secrets. Host
  code is a single static Go binary precisely so no daemon interprets network
  input and spawns children (main.go header, AGENTS.md "HOST = Go" gotcha).

## Prioritized threat table

| # | Threat | STRIDE | Likelihood | Impact | Mitigation (required) |
|---|--------|--------|-----------|--------|----------------------|
| T1 | Third-party go-plugin binary = arbitrary code execution on host as the user, with access to every credential the supervisor holds | Elevation of Privilege, Tampering | High if override is open by default | Critical (full host + all creds) | Plugins OFF by default; load only from an explicit user-configured allowlist of pinned paths + SHA-256; require cosign/minisign signature from a trusted key; never auto-discover plugins from PATH, cwd, kit, or download |
| T2 | Credential-broker-as-plugin exposes RAW secrets over plugin RPC (refresh token, op secret, provider key) instead of minted short-lived tokens | Information Disclosure | Medium | Critical | Broker RPC contract MUST return only short-lived minted tokens (mirror `gwstoken.mint()` `gwstoken.go:76`); raw creds never cross the plugin boundary; broker plugins are a separate, higher-trust allowlist than feature plugins |
| T3 | "go-plugin everything" reintroduces the exact daemon-spawns-child-from-network-input shape the project deliberately avoided (EDR/backdoor concern) | Elevation of Privilege | High (architectural) | High | See verdict. If pursued: the supervisor spawns plugins ONCE at startup from a static pinned manifest, never in response to network/VM input; no request-triggered spawns; document the EDR posture change |
| T4 | Installer `curl\|sh` fetches a tampered/MITM binary that then daemonizes and can register MCP servers the gateway later spawns with `op run` | Tampering, EoP | Medium | Critical | Pin release by tag + publish SHA-256 + verify before exec; verify GitHub release provenance (cosign/attestations, min. checksum file over TLS-pinned host); print+confirm, no silent daemonize; NO sudo; installer MUST NOT broker 1Password or write op-refs |
| T5 | Prompt-injected / compromised VM agent hits unauthenticated `memory:11435` (`memory.go:9`) — reads/rewrites durable memory (persistent injection) or `gws-token:11441` to mint a Google bearer | Spoofing, Tampering, Info Disclosure | Medium-High (agent is full-auto) | High (persistent memory poisoning; live Google bearer) | Require a per-sandbox bearer even locally (gws-token already supports `GWS_TOKEN_AUTH` `gwstoken.go:128`; make it MANDATORY and add the same to memory); token injected as a sandbox secret, not embedded in the image; scope memory writes/rate-limit |
| T6 | Malicious published mixin kit injects a hostile extension, skill, or network allow rule (adds an exfil domain to `allowedDomains`) | Tampering, EoP, Info Disclosure | Medium | High | Treat third-party kits as untrusted code: pin by digest, show a diff of injected extensions + network rules before first use, require explicit opt-in; never let a kit widen the network allowlist silently; extensions run in the VM but can drive host services (see T5) |
| T7 | OKF / knowledge bundle carries prompt-injection content the consuming agent ingests as instructions | Tampering (indirect prompt injection) | Medium | Medium-High | Treat ingested bundle content as DATA not INSTRUCTIONS; provenance-tag on ingest (see `ingest` skill); never auto-execute tool calls derived purely from bundle text; sign/pin the bundle source |
| T8 | Installer or setup writes secrets to disk in cleartext or logs them (op-refs, tokens) | Info Disclosure | Medium | High | `pi-stack setup` writes only op:// references (Makefile pattern), never resolved secrets; 0600 perms; no secret echo; op resolution stays at gateway spawn time |
| T9 | Plugin RPC transport (go-plugin default = local TCP) is reachable by other local processes / the VM, allowing a non-plugin to call broker RPC | Spoofing, Info Disclosure | Medium | High | Force Unix-domain sockets with 0600 owner-only, or go-plugin mTLS with AutoMTLS; never a routable/loopback-TCP handshake a co-resident process can grab |
| T10 | Supervisor loads a plugin whose version/ABI drifts, or a downgrade attack swaps a fixed plugin for a known-vulnerable pinned one | Tampering | Low-Medium | Medium | Pin plugin by digest not just version; refuse unknown protocol versions (go-plugin handshake); record loaded digests in a lockfile |
| T11 | `serve` / setup binds a service to a routable interface by env override, exposing brokers to the LAN | Info Disclosure, Spoofing | Low | Critical | Keep loopback default (`serve.go:31-33`); refuse non-loopback bind unless auth token is set AND explicitly acknowledged; the memory.go header warning becomes an enforced guard |
| T12 | `pi-stack setup` auto-registering MCP servers wires an attacker-chosen command the gateway later spawns with the user's op session | Tampering, EoP | Low-Medium | High | setup only registers servers from the built-in/allowlisted set; show the exact `sbx mcp add` command + resolved binary path for confirmation; never register from kit-supplied data |

## Non-negotiable security requirements

1. **Plugins are opt-in and pinned.** No plugin loads unless the user has
   listed it in an explicit local manifest by absolute path + SHA-256 digest.
   No auto-discovery from PATH, cwd, kits, or the network. Default state =
   zero third-party plugins.
2. **Plugins are signature-verified.** Each plugin binary is verified against a
   user-trusted signing key (cosign/minisign) before spawn; verification
   failure is fatal, not a warning.
3. **Brokers never emit raw secrets over RPC.** The credential-plugin contract
   returns only short-lived, minted, least-privilege tokens (the existing
   `gwstoken.mint()` model, `gwstoken.go:76`). Raw refresh tokens, op secrets,
   and provider keys never cross the plugin boundary. Broker plugins sit on a
   separate, stricter allowlist than feature plugins.
4. **Plugins run as the invoking user, never elevated.** No setuid, no sudo,
   no privileged capability. The installer never requests root.
5. **Spawn is startup-time and static, not request-time.** The supervisor
   spawns plugins once from a pinned manifest at start. It MUST NOT spawn or
   re-exec a plugin in response to VM/network input — that is the exact
   backdoor shape the Go-only convention exists to avoid (AGENTS.md).
6. **Plugin RPC is owner-only local.** Unix-domain socket 0600 or go-plugin
   AutoMTLS. Never a plain loopback-TCP handshake a co-resident process (incl.
   the VM's forwarded ports) can hijack.
7. **Host services authenticate the VM even locally.** memory (`memory.go:9`,
   currently unauthenticated) and gws-token (`gwstoken.go:128`, currently
   optional/empty) MUST require a per-sandbox bearer token, injected as a
   sandbox secret, before the redesign ships. Loopback is not treated as
   authentication.
8. **Loopback bind is enforced.** Non-loopback bind refuses to start unless an
   auth token is set and the operator explicitly acknowledges (upgrade the
   `memory.go` header warning to a runtime guard).
9. **Installer integrity + no silent side effects.** `curl|sh` verifies a
   published SHA-256 (and ideally cosign attestation) before executing; runs
   without sudo; does NOT daemonize silently, does NOT broker 1Password, does
   NOT write resolved secrets. It prints what it will do and asks.
10. **setup writes references, not secrets.** `pi-stack setup` persists only
    op:// refs / sandbox-secret handles at 0600, never resolved credential
    material; MCP registration is limited to the built-in allowlisted set and
    shown for confirmation.
11. **Third-party kits are untrusted code.** Pinned by digest; injected
    extensions and any network-allowlist additions are diffed and require
    explicit opt-in on first use; a kit can never silently widen
    `network.allowedDomains` (`spec.yaml`).
12. **Ingested knowledge is data, not instructions.** OKF/bundle content is
    provenance-tagged on ingest and never auto-triggers tool execution; the
    bundle source is signed/pinned.

## Verdict on "go-plugin everything, including credential proxies"

**Needs guardrails first — do NOT ship the open-override design as-is.**

- Making **feature** services (memory, MCP shims) go-plugin is acceptable IF
  requirements 1, 2, 4, 5, 6, 10 are met. It is a manageable, bounded change.
- Making the **credential broker** override-able by third-party plugins is the
  single highest-risk element of the redesign. It is defensible ONLY under the
  full non-negotiable set — especially #3 (minted tokens only), the stricter
  broker allowlist, #6 (owner-only RPC), and #2 (signed). Without those it is a
  direct path to whole-machine credential compromise (T1+T2) and should be
  rejected.
- Honest assessment of the EDR concern: **yes, "go-plugin everything"
  partially reintroduces the daemon-spawns-child shape the project chose Go to
  avoid** (main.go header, AGENTS.md). go-plugin's supervisor exists to fork
  helper executables and speak RPC to them. The mitigating distinction is #5:
  if spawns are static/startup-time from a pinned, signed manifest and never
  driven by network/VM input, the process tree is deterministic and defensible
  to endpoint security. If any plugin spawn ever becomes request-triggered, the
  project has recreated exactly the backdoor pattern it forbade, and the
  redesign should be stopped.

**Bottom line: PROCEED WITH GUARDRAILS.** The plugin architecture is viable for
feature services and even brokers, but only if the 12 non-negotiables above are
architectural preconditions, not follow-ups. The credential-broker plugin and
the request-time-spawn question are the two hard gates; get them wrong and the
host trust boundary collapses.
