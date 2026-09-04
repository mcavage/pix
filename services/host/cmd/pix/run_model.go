package main

import (
	"fmt"

	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/secret"
	"pix/host/workflow/launch"
)

// resolveRunModel keeps explicit and environment choices authoritative. When
// neither exists, it prevents Pi's own stale native default from taking over by
// selecting the shipped literal default for the first provider this PIX_HOME
// actually configures. It never chooses an unconfigured provider.
func resolveRunModel(explicit string, sidecar *envinfo.Sidecar, env hostenv.Env) (model, source string, err error) {
	if model, source = launch.SelectSessionModel(explicit, sidecar); model != "" {
		return model, source, nil
	}
	providers, state := secret.ConfiguredModelRefs(env)
	if state != secret.RefsAnswered {
		return "", "", fmt.Errorf("cannot choose a model because this PIX_HOME's secrets.env is unreadable; fix it or pass --model")
	}
	if len(providers) == 0 {
		return "", "", fmt.Errorf("no model selected and this PIX_HOME configures no provider; pass --model or set [models].main")
	}
	catalog, err := inference.LoadCatalog()
	if err != nil {
		return "", "", err
	}
	// The shipped top-level default remains OpenAI Sol when it is configured.
	// The rest are deterministic provider fallbacks, not a score or router.
	configured := make(map[string]bool, len(providers))
	for _, provider := range providers {
		configured[provider] = true
	}
	ordered := make([]string, 0, len(providers))
	for _, provider := range []string{"openai", "anthropic", "google"} {
		if configured[provider] {
			ordered = append(ordered, provider)
			delete(configured, provider)
		}
	}
	for _, provider := range providers {
		if configured[provider] {
			ordered = append(ordered, provider)
		}
	}
	model, err = inference.DefaultModelForProviders(catalog, ordered)
	if err != nil || model == "" {
		return model, "", err
	}
	return model, "configured provider default", nil
}
