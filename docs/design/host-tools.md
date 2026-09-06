# Pix host tools

Ordinary users create environments; maintainers develop Pix. Both use one
compiled-in MCP surface reached through the sbx Gateway. The agent remains in
its sandbox. `--dev` is explicit permission to run host commands as the host user.
It does not limit that authority to particular executables or directories.

The compiler registers the current Pix binary as a stdio server with fixed
home, workspace, sandbox, and development arguments. Its reserved name is
`pix-session-<stack-id>-<context-id>`. The context hash separates workspaces and
privilege levels; it is not a credential. The Gateway is host-global, so these
names do not provide confidentiality from other processes under the same login.

Every call checks the stored creation fingerprint, a fresh running sbx instance,
and its live reference locks. A process binds to its first verified instance and
cannot follow a recreated name. Authority changes cause fingerprint drift, so
reattachment cannot silently promote a normal session or inherit development
privileges. EOF or termination cancels outstanding jobs; jobs also monitor the
parent session and cancel if its proof disappears. After a proof-gated teardown
confirms the sandbox is absent, Pix removes its exact Gateway registration so
recreation starts a fresh server. Reset removes only this stack's registrations.

| Tool | Ordinary behavior |
| --- | --- |
| `pix_host_info` | Host workspace mapping and fixed privilege level |
| `pix_env_list` | Existing Pix environment listing |
| `pix_env_inspect` | Existing Pix show or effective preview |
| `pix_env_add` | Adopt a directory inside this session's workspace |
| `pix_env_test` | Run a bounded prompt in a separate host trial workspace |
| `pix_job_status` | Read this server's bounded, redacted job output and exit code |
| `pix_job_cancel` | Cancel this server's job |
| `pix_host_exec` | Available only with `--dev`; arbitrary explicit host argv |

The environment operations invoke the same Pix CLI that users invoke. There is
no alternate config writer, trust writer, environment grammar, or registry.
Adoption resolves symlinks and refuses sources outside the mounted workspace.
It neither overwrites an environment nor sets the default nor grants trust.
Inspection runs authored health probes only after the current environment
fingerprint has been approved; untrusted inspection reports reachability unknown.
Normal tools have no general command argument or privilege toggle.

Trials use private directories under `PIX_HOME/.state/trials`, outside their
environment source. They retain normal trust checks and do not receive host
tools. Pi JSON output supplies actual response metadata for model tests. Trial
workspace files remain available as evidence; ordinary Pix session teardown
removes its sandbox when it can prove safe removal.

Each server permits four running jobs and 64 jobs total. Jobs capture at most
64 KiB of output in memory, with truncation reported. No resolved credentials
are deliberately inherited; the environment passes only ordinary host runtime
settings and the fixed `PIX_HOME`. The existing diagnostic redactor runs before
output is disclosed. This is defense in depth, not a confidentiality guarantee
against a development command deliberately reading the host user's files.

The `environments` skill guides ordinary authoring and trials. The `pix-dev`
skill guides host builds and UAT, using an explicitly separate test home.
