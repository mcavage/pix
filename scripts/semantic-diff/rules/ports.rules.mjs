// W0 pins: reserved host-service ports. These ports are load-bearing across
// memory, the plugin subprocess env allowlist, and every extension that
// reaches host.docker.internal — a silent renumber in only one of these
// places is exactly the kind of drift a normal build/test pass would not
// catch (nothing round-trips a port number through a type system). The
// broker slot (BROKER_PORT / PIX_BROKER_PORT) was retired along with the
// rest of dormant host-mode broker (W2/U03B) and is deliberately no longer
// pinned here — see intended-changes.json for the waiver that landed with
// that removal.
export default [
	{
		id: "ports.reserved.identity",
		description: "identity.go's application-level readiness probe is pinned to :11435 (memory). The :11436 (knowledge) probe was retired with the built-in knowledge service, W2 U03A.",
		checks: [
			{
				file: "services/host/identity.go",
				kind: "contains",
				values: ['servicePort("MEMORY_PORT", 11435)'],
			},
		],
	},
	{
		id: "ports.reserved.serve",
		description: "serve.go's supervisor binds memory to its reserved default port (knowledge and broker are both retired capabilities that no longer bind).",
		checks: [
			{
				file: "services/host/serve.go",
				kind: "contains",
				values: ['env("MEMORY_PORT", "11435")'],
			},
		],
	},
	{
		id: "ports.reserved.memory-daemon-default",
		description: "The standalone memory daemon (runMemory) defaults to the same :11435 as serve.go's in-process path.",
		checks: [
			{
				file: "services/host/memory.go",
				kind: "contains",
				values: ['env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")'],
			},
		],
	},
	{
		id: "ports.reserved.plugin-env-allowlist",
		description: "serve_plugin.go's pluginEnvAllow (the ONLY env an external plugin subprocess inherits, F2) still carries every port-related variable a plugin needs to bind/discover the reserved ports (KNOWLEDGE_PORT/PIX_KNOWLEDGE_PORT were retired with the built-in knowledge service, W2 U03A; PIX_BROKER_PORT with the dormant broker slot, W2 U03B).",
		checks: [
			{
				file: "services/host/serve_plugin.go",
				kind: "contains",
				region: { start: "var pluginEnvAllow = []string{", end: "\n}\n" },
				values: ['"MEMORY_PORT"', '"PIX_MEMORY_PORT"'],
			},
		],
	},
];
