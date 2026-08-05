// probes.go — the ONE probe setup owns that no other surface needs. It classifies
// the way health's own probes do: a boundary that did not ANSWER is unknown, and
// only a positive answer may be absent and hand out a repair command. The loop
// applies verified gaps only, so a misclassified unknown here is the one way
// setup could mutate a host on a guess. The provider-key check is
// health.ProviderKeyProbe in AnyOf mode — the probe doctor reports from.
package provision

import (
	"context"
	"fmt"
	"strings"

	"pix/host/health"
	"pix/host/hostenv"
)

// ollamaModelsProbe answers "are the local model weights this host is configured to use actually on disk". Ollama is optional: a host without it runs Pix fine with memory off, so this probe is never required and a missing daemon is unknown, not a gap — there is nothing to pull weights into.
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

// missingLocalModels returns the configured tags `ollama list` positively did NOT
// report. An error means the listing could not be believed at all — the caller's
// cue to say unknown and pull nothing, because pulling a tag we could not prove
// missing is how a probe failure turns into a download.
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
