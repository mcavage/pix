// Test stub for @earendil-works/pi-ai (see tests/stub-loader.mjs).
export function StringEnum(values, opts = {}) {
	return { ...opts, type: "string", enum: [...values] };
}

export const builtinModels = new Map();
export function getBuiltinModel(provider, id) { return builtinModels.get(`${provider}/${id}`); }
