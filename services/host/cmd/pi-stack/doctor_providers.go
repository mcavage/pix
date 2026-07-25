package main

// secretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK.
func secretCheck(label, key, sbxOut string, sbxOK bool) check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return check{label: label, verdict: verdictTodo, detail: "sbx unavailable here (set on the host)", todo: cmd}
	}
	if grepWord(sbxOut, key) {
		return check{label: label, verdict: verdictReady, detail: "set"}
	}
	return check{label: label, verdict: verdictTodo, detail: "not set", todo: cmd}
}

// providersGroup builds the provider-secrets cluster: the model/github keys
// injected proxy-side (never visible in the VM), read via `sbx secret ls`.
func providersGroup(sbxOut string, sbxOK bool) group {
	providers := group{title: "Providers / keys (proxy-injected, never in the VM)"}
	for _, p := range []struct{ label, key string }{
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"google", "google"},
		{"github", "github"},
	} {
		providers.checks = append(providers.checks, secretCheck(p.label, p.key, sbxOut, sbxOK))
	}
	return providers
}
