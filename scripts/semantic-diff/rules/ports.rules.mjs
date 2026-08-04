// W0 pins: reserved host-service ports. These three ports are load-bearing
// across memory, knowledge, the dormant broker slot, the plugin subprocess
// env allowlist, and every extension that reaches host.docker.internal — a
// silent renumber in only one of these places is exactly the kind of drift a
// normal build/test pass would not catch (nothing round-trips a port number
// through a type system).
export default [
	{
		id: "ports.reserved.identity",
		description: "identity.go's application-level readiness probes are pinned to :11435 (memory) and :11436 (knowledge).",
		checks: [
			{
				file: "services/host/identity.go",
				kind: "contains",
				values: ['servicePort("MEMORY_PORT", 11435)', 'servicePort("KNOWLEDGE_PORT", 11436)'],
			},
		],
	},
	{
		id: "ports.reserved.serve",
		description: "serve.go's supervisor binds memory/knowledge/broker to their reserved default ports.",
		checks: [
			{
				file: "services/host/serve.go",
				kind: "contains",
				values: ['env("MEMORY_PORT", "11435")', 'env("KNOWLEDGE_PORT", "11436")', 'env("BROKER_PORT", "11437")'],
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
		description: "serve_plugin.go's pluginEnvAllowlist (the ONLY env an external plugin subprocess inherits, F2) still carries every port-related variable a plugin needs to bind/discover the reserved ports.",
		checks: [
			{
				file: "services/host/serve_plugin.go",
				kind: "contains",
				region: { start: "var pluginEnvAllowlist = map[string]bool{", end: "\n}\n" },
				values: ['"MEMORY_PORT":', '"KNOWLEDGE_PORT":', '"PIX_MEMORY_PORT":', '"PIX_KNOWLEDGE_PORT":', '"PIX_BROKER_PORT":'],
			},
		],
	},
];
