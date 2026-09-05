// Shared additive `roster` reader for the generated inference.json v1
// manifest (docs/design/environments.md §7). `extensions/inference.ts`,
// `extensions/subagents.ts`, and `extensions/ollama-bridge.ts` all read the
// SAME generated file; this module is the one place that turns its optional
// `roster` key into a typed value, so the three readers can never disagree
// about its shape.
//
// roster is additive: an older manifest (no `roster` key at all), a newer
// host emitting a shape this build does not understand, or outright
// corruption must all degrade to "no roster" — never throw, never partially
// trust a malformed value. A caller that gets `undefined` back treats it
// exactly like an absent file: every agent falls through to its next
// resolution step (see extensions/subagents.ts's model precedence).
export type Roster = {
	/** The selected environment's `[models].main` model id. */
	main: string;
	/**
	 * Agent name -> model id, from the selected environment's `[agents]`
	 * table plus any shipped-agent default the host already filled in
	 * (docs/design/environments.md §6.4). Ids are the exact, fully-qualified
	 * `provider/id` strings the manifest's own `models[]` (and therefore the
	 * providers `extensions/inference.ts`/`extensions/ollama-bridge.ts`
	 * register) already use — never re-derived or re-qualified here.
	 */
	agents: Record<string, string>;
};

// parseRoster validates and normalizes the `roster` value out of an ALREADY
// JSON.parse()'d inference.json document. It never throws: any shape
// mismatch (missing key, wrong type, a non-string agent model id) returns
// `undefined`, identical to an absent roster. This is the one seam every
// reader shares, so "absent" and "malformed" are indistinguishable to a
// caller by design — both mean "no roster is in effect".
export function parseRoster(parsed: unknown): Roster | undefined {
	if (!parsed || typeof parsed !== "object") return undefined;
	const rawRoster = (parsed as Record<string, unknown>).roster;
	if (!rawRoster || typeof rawRoster !== "object") return undefined;
	const rawMain = (rawRoster as Record<string, unknown>).main;
	if (typeof rawMain !== "string" || rawMain.trim() === "") return undefined;
	const agents: Record<string, string> = {};
	const rawAgents = (rawRoster as Record<string, unknown>).agents;
	if (rawAgents && typeof rawAgents === "object") {
		for (const [name, model] of Object.entries(rawAgents as Record<string, unknown>)) {
			if (typeof model === "string" && model.trim() !== "") agents[name] = model;
		}
	}
	return { main: rawMain.trim(), agents };
}
