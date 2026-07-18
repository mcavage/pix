// Test stub for typebox (see tests/stub-loader.mjs).
const make =
	(kind) =>
	(...args) => ({ kind, args });
export const Type = new Proxy(
	{},
	{
		get: (_t, prop) => make(String(prop)),
	},
);
