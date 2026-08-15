package models

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys/systest"
)

// This file closes the QA-flagged test-quality gaps in package models, which
// had NO test files at all before this pass — so ollamaTagsTimeout's own doc
// comment ("a hermetic test can shrink it") named a seam nothing ever
// exercised. Every test here is white-box (package models), deliberately: the
// daemon-error surface and the local/cloud classification carry-through both
// need unexported seams (ollamaListing, classifyOllamaTag, ollamaTagsTimeout,
// ollamaTagsBodyCap) no external package can reach.

// tagRow is one row this file's fake /api/tags servers emit.
type tagRow struct {
	Name       string `json:"name"`
	RemoteHost string `json:"remote_host,omitempty"`
	Size       int64  `json:"size"`
}

func tagsBody(rows ...tagRow) []byte {
	b, _ := json.Marshal(map[string]any{"models": rows})
	return b
}

// ollamaEnvServing points OLLAMA_HOST at handler and wires the darwin memory
// seam (sysctl) to totalGB, matching the pattern cmd/pix's own ollamaListEnv
// uses — kept local here since this package must not import cmd/pix.
func ollamaEnvServing(t *testing.T, handler http.HandlerFunc, totalGB float64) hostenv.Env {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil }}}
	systest.Of(env.System).GetenvFn = func(name string) string {
		if name == "OLLAMA_HOST" {
			return u.Host
		}
		return ""
	}
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		if name == "sysctl" {
			return fmt.Sprintf("%d\n", int64(totalGB*inference.BytesPerGB)), false, nil
		}
		return "", false, fmt.Errorf("unexpected command %s", name)
	}
	systest.Of(env.System).ReadFileFn = func(path string) (string, error) {
		if path == "/proc/meminfo" {
			return fmt.Sprintf("MemFree:  1024 kB\nMemTotal:       %d kB\nSwapTotal: 0 kB\n", int64(totalGB*inference.BytesPerGB/1024)), nil
		}
		return "", fmt.Errorf("unexpected file %s", path)
	}
	return env
}

func ollamaEnvTags(t *testing.T, totalGB float64, rows ...tagRow) hostenv.Env {
	t.Helper()
	return ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tagsBody(rows...))
	}, totalGB)
}

func ollamaBackendCfg(models ...config.InferenceModelBinding) *config.Config {
	return &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
		},
		Models: models,
	}}
}

// ---------------------------------------------------------------------------
// BLOCKER 2 — the response cap.

// TestOllamaListingReadsAValidBodyPastTheOldOneMiBCap pins the fix: a real
// daemon with a large-but-legitimate /api/tags body (many pulled tags) must be
// read successfully, not truncated into a decode failure. Reintroducing
// io.LimitReader(resp.Body, 1<<20) fails this test — the fixture is deliberately
// sized past that OLD cap, comfortably under the new one.
func TestOllamaListingReadsAValidBodyPastTheOldOneMiBCap(t *testing.T) {
	var rows []tagRow
	total := 0
	for i := 0; total < (1 << 20); i++ {
		row := tagRow{Name: fmt.Sprintf("model-%05d:latest", i), Size: 6_600_000_000}
		rows = append(rows, row)
		total += len(row.Name) + 64 // rough per-row footprint, doesn't need to be exact
	}
	env := ollamaEnvTags(t, 64, rows...)
	seen, err := ollamaListing(env)
	if err != nil {
		t.Fatalf("a valid body past the OLD 1 MiB cap must still be read: %v", err)
	}
	if len(seen) != len(rows) {
		t.Fatalf("got %d tags, want %d", len(seen), len(rows))
	}
}

// TestOllamaListingRefusesABodyPastTheNewCap: the cap still exists, and hitting
// it must be reported AS a cap, not folded into "could not list" — a truncated
// response and a malformed one are different facts about a healthy daemon.
func TestOllamaListingRefusesABodyPastTheNewCap(t *testing.T) {
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A single oversized value blows the cap without needing millions of rows.
		big := strings.Repeat("a", ollamaTagsBodyCap+1)
		_, _ = fmt.Fprintf(w, `{"models":[{"name":"x","size":%s0}]}`, big)
	}, 64)
	_, err := ollamaListing(env)
	if err == nil {
		t.Fatal("a body past the cap must be refused, not silently truncated into an empty/partial listing")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want it to name the cap specifically, not a generic failure", err)
	}
}

// ---------------------------------------------------------------------------
// TEST-QUALITY GAP — daemon error surface (previously untested: this package
// had no test files at all).

func TestOllamaListingNonTwoXXStatus(t *testing.T) {
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, 64)
	_, err := ollamaListing(env)
	if err == nil {
		t.Fatal("a non-2xx status must be refused")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want the HTTP status named", err)
	}
}

func TestOllamaListingMalformedJSON(t *testing.T) {
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models": [ this is not json`))
	}, 64)
	_, err := ollamaListing(env)
	if err == nil {
		t.Fatal("malformed JSON from a 200 response must be refused, not silently treated as empty")
	}
}

// TestOllamaListingHungDaemonRespectsTheShrinkableTimeout is the seam
// ollamaTagsTimeout's own doc comment promises ("a var, not a const, so a
// hermetic test can shrink it") and which nothing in this package's test suite
// exercised before this file existed.
func TestOllamaListingHungDaemonRespectsTheShrinkableTimeout(t *testing.T) {
	orig := ollamaTagsTimeout
	ollamaTagsTimeout = 50 * time.Millisecond
	defer func() { ollamaTagsTimeout = orig }()

	block := make(chan struct{})
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds within the test's lifetime
	}, 64)
	// Registered AFTER ollamaEnvServing, which registers its own httptest
	// server Close cleanup. Cleanups run LIFO, so this unblocks the parked
	// handler BEFORE Close waits on it. Registering it first deadlocks the
	// whole package for the full go-test timeout: Close blocks on an
	// outstanding request that is itself waiting on a channel only a
	// later-running cleanup would close.
	t.Cleanup(func() { close(block) })

	start := time.Now()
	_, err := ollamaListing(env)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a daemon that never answers must be reported, not hung on forever")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ollamaListing took %s to time out a 50ms-bounded request — the shrunk timeout was not honored", elapsed)
	}
}

// ---------------------------------------------------------------------------
// BLOCKER 3(a) — the RAM gate for an unknown local tag.

// TestConfigureOllamaInferenceGatesUnknownLocalTagByDiskSize: an unknown tag
// classified LOCAL by its on-disk size must ALSO be measured against usable
// RAM before it is bound and narrated as "bounded by this machine's RAM" — the
// exact claim the old unconditional bind() made with nothing behind it.
func TestConfigureOllamaInferenceGatesUnknownLocalTagByDiskSize(t *testing.T) {
	cfg := &config.Config{}
	// 32GB total on darwin -> ~21.4GB usable (0.67 fraction below the 36GB tier).
	// A 100GB "model" cannot fit under any real machine's usable RAM.
	env := ollamaEnvTags(t, 32,
		tagRow{Name: "qwen3.5:9b", Size: 6_600_000_000},                    // catalog: keeps this run from rolling back to empty
		tagRow{Name: "monster:70b-huge", Size: 100 * inference.BytesPerGB}, // unknown, huge, must be RAM-gated
	)
	var out strings.Builder
	plan, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Membership, not equality: the CATALOG arm independently reports every
	// registry model that cannot fit this machine (qwen3.5:27b/35b here),
	// whether or not it was ever pulled. Asserting the exact slice would pin
	// the shipped catalog's contents into an unrelated test and break on the
	// next models.json edit. What this test owns is the UNKNOWN tag.
	if !slices.Contains(plan.SkippedRAM, "ollama/monster:70b-huge") {
		t.Fatalf("plan.SkippedRAM = %v, want it to contain ollama/monster:70b-huge", plan.SkippedRAM)
	}
	if len(plan.UnknownLocal) != 0 {
		t.Fatalf("a RAM-gated tag must never also appear in UnknownLocal: %v", plan.UnknownLocal)
	}
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/monster:70b-huge" {
			t.Fatalf("a tag that failed the RAM gate must never be bound: %+v", b)
		}
	}
	if strings.Contains(out.String(), "bound ollama/monster:70b-huge") {
		t.Fatalf("must never claim to have bound a RAM-gated tag: %q", out.String())
	}
}

// TestConfigureOllamaInferenceNeverClaimsAnUnmeasuredRAMGate: on a machine this
// package could NOT size (Memory.OK == false), the printed line for a bound
// unknown-local tag must not claim a RAM fit it never checked — per AGENTS.md
// invariant 12, a success word (here, "fits ... usable RAM") is earned by a
// probe, or it must not be said at all.
func TestConfigureOllamaInferenceNeverClaimsAnUnmeasuredRAMGate(t *testing.T) {
	cfg := &config.Config{}
	// An unsupported "goos" inside ProbeHostMemory always answers OK:false; the
	// simplest fixture for that here is a sysctl/proc read that both fail.
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tagsBody(tagRow{Name: "monster:70b-huge", Size: 5 * inference.BytesPerGB}))
	}, 0)
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		return "", false, fmt.Errorf("sysctl unavailable")
	}
	systest.Of(env.System).ReadFileFn = func(path string) (string, error) {
		return "", fmt.Errorf("no /proc/meminfo")
	}
	var out strings.Builder
	plan, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Memory.OK {
		t.Fatal("test fixture bug: expected an unmeasurable machine")
	}
	if strings.Contains(out.String(), "usable RAM)") && strings.Contains(out.String(), "fits") {
		t.Fatalf("must never claim a fit that was never measured: %q", out.String())
	}
	if !strings.Contains(out.String(), "could not be measured") {
		t.Fatalf("an unmeasured machine must say so, not stay silent about the gap: %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// BLOCKER 3(b) — classification carries through to the probe split.

// TestVerifyOllamaInferenceClassifiesUnknownLocalTagIntoTheSerializedLoop pins
// the fix: an unknown-catalog binding that ConfigureOllamaInference classified
// LOCAL must be probed through the SAME serialized, budgeted path a catalog
// local rung uses (OllamaLocalProbeTimeout, one at a time), never lumped into
// the concurrent cloud bucket (ollamaCloudProbeTimeout) alongside every other
// cold-load — the exact RAM-exhaustion hazard the local loop's serialization
// exists to prevent. Reintroducing the registry-only split (`if m, found :=
// reg.Get(c.label); found && m.Local`) with no unknown-tag branch fails here.
func TestVerifyOllamaInferenceClassifiesUnknownLocalTagIntoTheSerializedLoop(t *testing.T) {
	cfg := ollamaBackendCfg(
		config.InferenceModelBinding{Model: "ollama/monster:70b-instruct", Backend: "ollama", Upstream: "monster:70b-instruct", Available: true},
		config.InferenceModelBinding{Model: "ollama/minimax-m3:cloud", Backend: "ollama", Upstream: "minimax-m3:cloud", Available: true},
	)
	env := ollamaEnvTags(t, 64,
		tagRow{Name: "monster:70b-instruct", Size: 40 * inference.BytesPerGB}, // no remote_host, no :cloud suffix, GB-scale -> LOCAL
		tagRow{Name: "minimax-m3:cloud", RemoteHost: "https://ollama.com", Size: 300},
	)
	var mu sync.Mutex
	timeoutFor := map[string]time.Duration{}
	numCtxFor := map[string]int{}
	env.OllamaInference = func(endpoint, model string, numCtx int, timeout time.Duration) error {
		mu.Lock()
		timeoutFor[model], numCtxFor[model] = timeout, numCtx
		mu.Unlock()
		return nil
	}

	res, err := VerifyOllamaInference(cfg, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified != 2 {
		t.Fatalf("Verified = %d, want 2 (%+v)", res.Verified, res)
	}
	if got := timeoutFor["monster:70b-instruct"]; got != OllamaLocalProbeTimeout {
		t.Fatalf("unknown-but-local tag probed with timeout %s, want the LOCAL budget %s (it went through the concurrent cloud path instead)", got, OllamaLocalProbeTimeout)
	}
	if got := timeoutFor["minimax-m3:cloud"]; got != ollamaCloudProbeTimeout {
		t.Fatalf("unknown-cloud tag probed with timeout %s, want the CLOUD budget %s", got, ollamaCloudProbeTimeout)
	}
	if got := numCtxFor["monster:70b-instruct"]; got != 0 {
		t.Fatalf("an unknown local tag has no declared context window, so num_ctx must stay 0: got %d", got)
	}
}

// ---------------------------------------------------------------------------
// MEDIUM 4 — untrusted tag names at the ingestion boundary.

// TestOllamaListingDropsANameCarryingAnsiEraseAndNewline: a rogue listener (or
// a redirected OLLAMA_HOST) fully controls m.Name; a name that could forge or
// erase a rendered line must never survive ollamaListing.
func TestOllamaListingDropsANameCarryingAnsiEraseAndNewline(t *testing.T) {
	evil := "qwen3.5:9b\x1b[2K\rbound ollama/evil as LOCAL (free and private)\n"
	env := ollamaEnvTags(t, 64,
		tagRow{Name: "qwen3.5:9b", Size: 6_600_000_000},
		tagRow{Name: evil, Size: 6_600_000_000},
	)
	seen, err := ollamaListing(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen[evil]; ok {
		t.Fatalf("a name failing Ollama's own grammar must be dropped at ingestion, got: %v", seen)
	}
	if len(seen) != 1 {
		t.Fatalf("seen = %v, want exactly the one conforming tag", seen)
	}
}

// TestConfigureOllamaInferenceNeverRendersAnInjectedName is the full-path
// version: the malicious row must never reach cfg or the printed narration.
func TestConfigureOllamaInferenceNeverRendersAnInjectedName(t *testing.T) {
	evil := "totally-local:9b\x1b[2K\rbound ollama/evil as LOCAL (free and private)\n"
	cfg := &config.Config{}
	env := ollamaEnvTags(t, 64,
		tagRow{Name: "qwen3.5:9b", Size: 6_600_000_000},
		tagRow{Name: evil, Size: 6_600_000_000},
	)
	var out strings.Builder
	_, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r") {
		t.Fatalf("rendered output must never carry a raw ANSI/CR byte from an untrusted tag name: %q", out.String())
	}
	for _, b := range cfg.Inference.Models {
		if strings.ContainsAny(b.Model, "\x1b\r\n") || strings.ContainsAny(b.Upstream, "\x1b\r\n") {
			t.Fatalf("a persisted binding must never carry control bytes from an untrusted name: %+v", b)
		}
	}
}

// ---------------------------------------------------------------------------
// MEDIUM 5 — idempotent narration.

// TestConfigureOllamaInferenceSecondRunNarratesNoNewBindings: a read-only
// second pass over the same listing must not print "bound X" again for a tag
// it did not touch this time.
func TestConfigureOllamaInferenceSecondRunNarratesNoNewBindings(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaEnvTags(t, 64, tagRow{Name: "llama5.1:70b-instruct", Size: 6_600_000_000})

	var first strings.Builder
	if _, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "bound ollama/llama5.1:70b-instruct") {
		t.Fatalf("the first run must narrate the new bind: %q", first.String())
	}

	var second strings.Builder
	if _, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &second); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.String(), "bound ollama/llama5.1:70b-instruct") {
		t.Fatalf("a second run that bound nothing new must not narrate a bind: %q", second.String())
	}
}

// ---------------------------------------------------------------------------
// TEST-QUALITY GAPS — classification precedence and boundaries.

// TestClassifyOllamaTagRemoteHostWinsOverLocalLookingNameAndSize is the
// isolated remote_host case: nothing else in this suite exercises `case
// info.RemoteHost != ""` without ALSO tripping the :cloud/-cloud suffix, so
// deleting that case broke zero tests before this one.
func TestClassifyOllamaTagRemoteHostWinsOverLocalLookingNameAndSize(t *testing.T) {
	cloud, classified := classifyOllamaTag("totally-local-name:9b", ollamaTagInfo{RemoteHost: "https://ollama.com", Size: 40 * inference.BytesPerGB})
	if !classified || !cloud {
		t.Fatalf("cloud=%v classified=%v, want cloud, classified (remote_host is authoritative over a local-looking name and a large size)", cloud, classified)
	}
}

// TestClassifyOllamaTagSuffixWinsOverSize: a :cloud name with a GB-scale size
// (a real Cloud row never reports one, but the RULE must not depend on it).
func TestClassifyOllamaTagSuffixWinsOverSize(t *testing.T) {
	cloud, classified := classifyOllamaTag("glm-5.2:cloud", ollamaTagInfo{Size: 40 * inference.BytesPerGB})
	if !classified || !cloud {
		t.Fatalf("cloud=%v classified=%v, want cloud, classified", cloud, classified)
	}
}

func TestClassifyOllamaTagSizeFloorBoundary(t *testing.T) {
	cases := []struct {
		name           string
		size           int64
		wantCloud      bool
		wantClassified bool
	}{
		{"one byte under the floor", ollamaLocalSizeFloor - 1, false, false},
		{"exactly the floor", ollamaLocalSizeFloor, false, true},
		{"size zero", 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud, classified := classifyOllamaTag("mystery:latest", ollamaTagInfo{Size: tc.size})
			if cloud != tc.wantCloud || classified != tc.wantClassified {
				t.Fatalf("classifyOllamaTag(size=%d) = (cloud=%v, classified=%v), want (%v, %v)", tc.size, cloud, classified, tc.wantCloud, tc.wantClassified)
			}
		})
	}
}

// TestOllamaListingSizeAbsentFromResponseDecodesAsZero: a row that omits
// "size" entirely (an older/minimal daemon) must decode cleanly to Size 0,
// exactly like an explicit zero — never a decode error, never a classification
// crash.
func TestOllamaListingSizeAbsentFromResponseDecodesAsZero(t *testing.T) {
	env := ollamaEnvServing(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"mystery:latest"}]}`))
	}, 64)
	seen, err := ollamaListing(env)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := seen["mystery:latest"]
	if !ok || info.Size != 0 {
		t.Fatalf("seen[mystery:latest] = %+v (ok=%v), want Size 0", info, ok)
	}
}

// TestConfigureOllamaInferenceUnknownTagDropsSilentlyWhenNotSelected pins a
// DELIBERATE decision: an unknown tag classified LOCAL when only Cloud was
// selected (and vice versa) is dropped with no report, no error, and no
// binding — the same silence the CATALOG loop already has for a catalog model
// outside the selected axis (`case m.Local && sel.Local` simply never matches
// otherwise). This is a design choice, pinned rather than left accidental: a
// tag outside the user's selection is exactly as uninteresting as a catalog
// model outside it, and reporting on it would be noise about an axis the user
// did not ask for.
func TestConfigureOllamaInferenceUnknownTagDropsSilentlyWhenNotSelected(t *testing.T) {
	t.Run("unknown LOCAL tag, only Cloud selected", func(t *testing.T) {
		cfg := &config.Config{}
		env := ollamaEnvTags(t, 64,
			tagRow{Name: "glm-5.2:cloud", RemoteHost: "https://ollama.com", Size: 300}, // catalog cloud, keeps the run non-empty
			tagRow{Name: "monster:70b-instruct", Size: 40 * inference.BytesPerGB},      // unknown LOCAL, but Local not selected
		)
		var out strings.Builder
		plan, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Cloud: true}, &out)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.UnknownLocal) != 0 || len(plan.UnknownUnclassified) != 0 || len(plan.SkippedRAM) != 0 {
			t.Fatalf("an unselected-axis tag must generate NO report at all: %+v", plan)
		}
		for _, b := range cfg.Inference.Models {
			if b.Model == "ollama/monster:70b-instruct" {
				t.Fatalf("must never bind a classified tag outside the selected axis: %+v", b)
			}
		}
		if strings.Contains(out.String(), "monster:70b-instruct") {
			t.Fatalf("must never mention a classified tag outside the selected axis: %q", out.String())
		}
	})

	t.Run("unknown CLOUD tag, only Local selected", func(t *testing.T) {
		cfg := &config.Config{}
		env := ollamaEnvTags(t, 64,
			tagRow{Name: "qwen3.5:9b", Size: 6_600_000_000}, // catalog local, keeps the run non-empty
			tagRow{Name: "minimax-m3:cloud", RemoteHost: "https://ollama.com", Size: 300},
		)
		var out strings.Builder
		plan, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, &out)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.UnknownCloud) != 0 {
			t.Fatalf("an unselected-axis cloud tag must generate NO report: %+v", plan)
		}
		for _, b := range cfg.Inference.Models {
			if b.Model == "ollama/minimax-m3:cloud" {
				t.Fatalf("must never bind a classified cloud tag when Cloud was not selected: %+v", b)
			}
		}
		if strings.Contains(out.String(), "minimax-m3:cloud") {
			t.Fatalf("must never mention a classified cloud tag outside the selected axis: %q", out.String())
		}
	})
}

// TestConfigureOllamaInferenceCatalogModelBothKnownAndListedBindsExactlyOnce:
// a catalog model that is BOTH in the registry AND on the daemon's listing
// must bind through the FIRST (catalog) pass only — the second pass's
// knownTags exclusion exists precisely to prevent a double bind through the
// unknown-tag path.
func TestConfigureOllamaInferenceCatalogModelBothKnownAndListedBindsExactlyOnce(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaEnvTags(t, 64, tagRow{Name: "qwen3.5:9b", Size: 6_600_000_000})
	plan, err := ConfigureOllamaInference(cfg, env, OllamaSelection{Local: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.UnknownLocal) != 0 {
		t.Fatalf("a catalog model must never also appear as an unknown-tag bind: %v", plan.UnknownLocal)
	}
	count := 0
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/qwen3.5:9b" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ollama/qwen3.5:9b bound %d time(s), want exactly 1", count)
	}
}
