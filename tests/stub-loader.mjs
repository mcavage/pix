// ESM resolve hook that maps the pi runtime packages (which only exist inside
// pi's own install, not this repo's node_modules) to local stubs, so tests can
// import extensions/subagents.ts under plain node. Registered from the test
// file via module.register().
const stubs = new Map([
	["@earendil-works/pi-ai", "./stubs/pi-ai.mjs"],
	["@earendil-works/pi-coding-agent", "./stubs/pi-coding-agent.mjs"],
	["@earendil-works/pi-tui", "./stubs/pi-tui.mjs"],
	["typebox", "./stubs/typebox.mjs"],
]);

export async function resolve(specifier, context, next) {
	const stub = stubs.get(specifier);
	if (stub) {
		return { url: new URL(stub, import.meta.url).href, shortCircuit: true };
	}
	return next(specifier, context);
}
