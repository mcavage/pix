// probes.go — the two probes setup owns that no other surface needs.
//
// Both classify the same way health's own probes do, and for the same reason:
// a boundary that did not ANSWER is unknown, and only an answer that positively
// identifies the gap may be absent and hand out a repair command. The loop then
// applies verified gaps only, so a misclassified unknown here would be the one
// way setup could mutate a host on a guess.
package provision

import (
	"context"
	"fmt"
	"strings"

	"pix/host/health"
	"pix/host/hostenv"
)

// --- local models -----------------------------------------------------------

// ollamaModelsProbe answers "are the local model weights this host is
// configured to use actually on disk". Ollama is optional: a host without it
// runs Pix fine with memory off, so this probe is never required and a missing
// daemon is unknown, not a gap — there is nothing to pull weights into.
type ollamaModelsProbe struct {
	Env  hostenv.Env
	Tags []string
}

func (ollamaModelsProbe) Name() string   { return "models" }
func (ollamaModelsProbe) Required() bool { return false }

func (p ollamaModelsProbe) Check(ctx context.Context) health.Result {
	name := p.Name()
	if len(p.Tags) == 0 {
		return health.Result{Name: name, Status: health.StatusReady, Detail: "no local models configured",
			Evidence: "config names no watcher/embed/bridge model"}
	}
	missing, err := missingLocalModels(ctx, p.Env, p.Tags)
	if err != nil {
		return health.Result{Name: name, Status: health.StatusUnknown, Detail: "could not list local models",
			Evidence: "ollama list: " + err.Error()}
	}
	if len(missing) == 0 {
		return health.Result{Name: name, Status: health.StatusReady,
			Detail:   fmt.Sprintf("%d local model(s) pulled", len(p.Tags)),
			Evidence: "ollama list contains " + strings.Join(p.Tags, ", ")}
	}
	return health.Result{Name: name, Status: health.StatusAbsent,
		Detail:   "missing " + strings.Join(missing, ", "),
		Fix:      "ollama pull " + missing[0],
		Evidence: "ollama list answered without " + strings.Join(missing, ", ")}
}

// missingLocalModels returns the configured tags `ollama list` positively did
// NOT report. An error means the listing could not be believed at all, which is
// the caller's cue to say unknown and pull nothing: pulling a tag we could not
// prove missing is how a probe failure turns into a download.
func missingLocalModels(ctx context.Context, env hostenv.Env, tags []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := env.LookPath("ollama"); err != nil {
		return nil, fmt.Errorf("ollama is not on PATH")
	}
	out, timedOut, err := env.RunTimed("ollama", "list")
	switch {
	case timedOut:
		return nil, fmt.Errorf("deadline exceeded")
	case err != nil:
		return nil, fmt.Errorf("exited non-zero")
	}
	var missing []string
	for _, tag := range tags {
		if !listsTag(out, tag) {
			missing = append(missing, tag)
		}
	}
	return missing, nil
}

// listsTag matches a tag against `ollama list` output. An entry with no
// explicit `:tag` is the `:latest` row, which is what a bare configured name
// refers to.
func listsTag(listing, tag string) bool {
	want := tag
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		got := fields[0]
		if !strings.Contains(got, ":") {
			got += ":latest"
		}
		if got == want {
			return true
		}
	}
	return false
}

// --- provider keys ----------------------------------------------------------

// providerKeysProbe answers "can this host call a model" from the key store,
// and it is TRI-STATE on purpose: only a store that ANSWERED and listed none of
// the provider keys is a no-key verdict. A store that failed, crashed or hung is
// unknown, so a transient `sbx secret ls` failure can never be mistaken for a
// missing key — and, because the loop applies verified gaps only, can never
// trigger a mutation either.
//
// It differs from health.ProviderKeyProbe in exactly one way, and it is the
// difference that matters here: ANY ONE key is enough to launch, so this is an
// any-of test, not an all-of one.
type providerKeysProbe struct {
	Env hostenv.Env
}

func (providerKeysProbe) Name() string   { return "providers" }
func (providerKeysProbe) Required() bool { return true }

func (p providerKeysProbe) Check(ctx context.Context) health.Result {
	name := p.Name()
	want := ProviderKeyEnvVars()
	if err := ctx.Err(); err != nil {
		return health.Result{Name: name, Status: health.StatusUnknown, Detail: "probe timed out",
			Evidence: "key listing: deadline exceeded"}
	}
	if _, err := p.Env.LookPath("sbx"); err != nil {
		return health.Result{Name: name, Status: health.StatusUnknown, Detail: "key store not available",
			Evidence: "the key-store command is not on PATH"}
	}
	out, timedOut, err := p.Env.RunTimed("sbx", "secret", "ls")
	switch {
	case timedOut:
		return health.Result{Name: name, Status: health.StatusUnknown, Detail: "probe timed out",
			Evidence: "key listing: deadline exceeded"}
	case err != nil && isDenied(out):
		return health.Result{Name: name, Status: health.StatusDenied, Detail: "key store refused the query",
			Fix: providerKeyFix(want[0]), Evidence: "key listing was refused"}
	case err != nil:
		return health.Result{Name: name, Status: health.StatusUnknown, Detail: "probe failed",
			Evidence: "key listing: exited non-zero"}
	}
	var have []string
	for _, w := range want {
		if listsSecret(out, w) {
			have = append(have, w)
		}
	}
	if len(have) > 0 {
		return health.Result{Name: name, Status: health.StatusReady,
			Detail:   fmt.Sprintf("%d key(s) wired", len(have)),
			Evidence: "key store lists " + strings.Join(have, ", ")}
	}
	return health.Result{Name: name, Status: health.StatusAbsent, Detail: "no provider key",
		Fix: providerKeyFix(want[0]), Evidence: "key store answered without " + strings.Join(want, ", ")}
}

// providerKeyFix names the ONE command that solicits a credential. Setup does
// not, which is why this is a fix string rather than a step with an Apply.
func providerKeyFix(envVar string) string {
	return "pix models add " + providerOfEnvVar(envVar)
}

func providerOfEnvVar(envVar string) string {
	return strings.ToLower(strings.TrimSuffix(envVar, "_API_KEY"))
}

// listsSecret looks for a whole-field match so `ANTHROPIC_API_KEY_OLD` cannot
// satisfy `ANTHROPIC_API_KEY`.
func listsSecret(listing, name string) bool {
	for _, field := range strings.Fields(strings.ReplaceAll(listing, "=", " ")) {
		if strings.Trim(field, "\"',:") == name {
			return true
		}
	}
	return false
}

// isDenied recognizes the store's own refusal words. A refusal is a POSITIVE
// answer ("you may not"), which is why it is denied rather than unknown — and
// why nothing here tries to repair it: no setup step can grant a permission.
func isDenied(out string) bool {
	low := strings.ToLower(out)
	for _, n := range []string{"permission denied", "forbidden", "not authorized", "unauthorized", "access denied"} {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}
