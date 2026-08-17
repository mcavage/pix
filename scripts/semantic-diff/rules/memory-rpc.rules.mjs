// W0 pins: the memory service's (:11435) JSON-RPC surface AS ACTUALLY CALLED
// BY EXTENSIONS. Each pin's "expected" is a fixed literal, not derived from
// either witness file — that is what lets it catch a codemod that renames a
// method (or a param) consistently across server AND client in one commit
// (lockstep corruption): both witnesses would still agree WITH EACH OTHER,
// but neither would agree with this pin anymore.
export default [
	{
		id: "memory.rpc.methods.server",
		description: "services/host/serve_plugin.go's JSON-RPC method table (memoryStoreMux, the ONE :11435 surface both the supervised unit and the bare daemon answer through) exposes exactly this method set.",
		checks: [
			{
				file: "services/host/serve_plugin.go",
				kind: "set",
				region: { start: "return jsonrpcMux(map[string]func(jsonObj) (any, error){", end: "\n\t})\n}" },
				pattern: '"([a-z]+)":\\s*with\\(func\\(',
				expected: ["forget", "health", "identity", "observe", "recall", "remember", "stats", "synthesize"],
			},
		],
	},
	{
		id: "memory.rpc.methods.client-witness",
		description: "extensions/memory-recall.ts only ever calls the memory RPC methods it is documented to use. A method appearing here that the server does not expose (or vice versa, see memory.rpc.methods.server) is a real contract break, not a refactor artifact.",
		checks: [
			{
				file: "extensions/memory-recall.ts",
				kind: "set",
				pattern: 'rpc\\("([a-z]+)"',
				expected: ["forget", "recall", "remember", "stats"],
			},
		],
	},
	{
		id: "memory.rpc.recall.params.server",
		description: "The server-side `recall` handler reads exactly these param names off the JSON-RPC request.",
		checks: [
			{
				file: "services/host/serve_plugin.go",
				kind: "set",
				region: { start: '"recall": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {', end: '\n\t\t"remember":' },
				patterns: ['getStr\\(p, "([a-zA-Z]+)"\\)', 'clampInt\\(p\\["([a-zA-Z]+)"\\]'],
				expected: ["query", "limit", "charBudget", "kind", "project"],
			},
			{
				// profileFromParams(p) reads "profile" internally (see
				// memory.rpc.profile.reader below); this just proves recall's
				// handler still routes profile through that shared reader instead
				// of reading p["profile"] directly (which would silently bypass
				// memNormProfile's default-bucket normalization).
				file: "services/host/serve_plugin.go",
				kind: "contains",
				region: { start: '"recall": with(func(s plugin.MemoryStore, p jsonObj) (any, error) {', end: '\n\t\t"remember":' },
				values: ["profileFromParams(p)"],
			},
		],
	},
	{
		id: "memory.rpc.profile.reader",
		description: "profile is normalized through memNormProfile(getStr(p, \"profile\")), not read raw — an unscoped caller must keep resolving to the default profile bucket.",
		checks: [
			{
				file: "services/host/memory.go",
				kind: "contains",
				region: { start: "func profileFromParams(p jsonObj) string {", end: "\n}\n" },
				values: ['memNormProfile(getStr(p, "profile"))'],
			},
		],
	},
	{
		id: "memory.rpc.recall.params.client-witness",
		description: "extensions/memory-recall.ts's primary recall() call still sends every param the server handler reads (memory.rpc.recall.params.server).",
		checks: [
			{
				file: "extensions/memory-recall.ts",
				kind: "contains",
				region: { start: 'rpc("recall", { query: prompt', end: "});" },
				values: ["project", "profile", "limit:", "charBudget:"],
			},
		],
	},
];
