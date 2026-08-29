package inference

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"pix/host/hostenv"
)

// hardware.go owns ONE fact — this machine's physical memory — for one purpose:
// sizing the LOCAL model it can be OFFERED. An inference informs offers and
// remediation; it can never produce a verdict (safety invariant 13). The
// reading is TOTAL memory, not free: free is a snapshot of an unrelated moment,
// so a machine with a browser open would be offered a smaller model forever.

// HostMemory is the probed physical-memory fact, the fraction of it a model
// runtime may plan on, and where the number came from. OK=false means the
// machine could not be sized: callers degrade to no offer, never up.
type HostMemory struct {
	TotalGB  float64
	UsableGB float64
	Source   string // "sysctl hw.memsize" | "/proc/meminfo MemTotal" | ""
	OK       bool
}

// BytesPerGB is 2^30 — the unit every number in the ladder is expressed in.
const BytesPerGB = 1 << 30

// darwinFractionTierGB is where macOS's default GPU wired-memory limit moves
// from ~2/3 of RAM to ~3/4. Apple never contracted it, so it is re-grounded
// like a scorecard number, not derived.
const darwinFractionTierGB = 36

// usableFraction is the share of TOTAL RAM a model runtime may plan on. Darwin
// is TWO-TIER because the Metal working-set ceiling is: a flat 0.75
// over-promises on the small unified-memory machines that can least afford it.
// Linux has no unified-memory guarantee, so it stays conservative.
func usableFraction(goos string, totalGB float64) (float64, bool) {
	switch goos {
	case "darwin":
		if totalGB > darwinFractionTierGB {
			return 0.75, true
		}
		return 0.67, true
	case "linux":
		return 0.60, true
	default:
		return 0, false
	}
}

// ProbeHostMemory reads TOTAL physical memory through the hostenv.Env seams, so
// it is fakeable in tests and never links cgo.
func ProbeHostMemory(env hostenv.Env) HostMemory {
	return probeHostMemoryFor(runtime.GOOS, env)
}

// probeHostMemoryFor is ProbeHostMemory with the OS injected, so one machine
// exercises both readers. darwin: `sysctl -n hw.memsize` (bytes). linux:
// /proc/meminfo MemTotal (kB). Any other GOOS: OK=false.
func probeHostMemoryFor(goos string, env hostenv.Env) HostMemory {
	var totalGB float64
	var source string
	switch goos {
	case "darwin":
		source = "sysctl hw.memsize"
		// Bounded seam: a wedged sysctl must never hang setup.
		out, timedOut, cmdErr := env.RunTimed("sysctl", "-n", "hw.memsize")
		if cmdErr != nil || timedOut {
			return HostMemory{Source: source}
		}
		bytes, parseErr := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if parseErr != nil || bytes <= 0 {
			return HostMemory{Source: source}
		}
		totalGB = bytes / BytesPerGB
	case "linux":
		source = "/proc/meminfo MemTotal"
		body, err := env.ReadFile("/proc/meminfo")
		if err != nil {
			return HostMemory{Source: source}
		}
		kb, ok := parseMemTotalKB(body)
		if !ok {
			return HostMemory{Source: source}
		}
		totalGB = kb * 1024 / BytesPerGB
	default:
		return HostMemory{}
	}
	fraction, ok := usableFraction(goos, totalGB)
	if !ok {
		return HostMemory{Source: source}
	}
	return HostMemory{TotalGB: totalGB, UsableGB: totalGB * fraction, Source: source, OK: true}
}

// parseMemTotalKB pulls MemTotal (in kB) out of /proc/meminfo.
func parseMemTotalKB(body string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || kb <= 0 {
			return 0, false
		}
		return kb, true
	}
	return 0, false
}

// LocalFloorTotalGB is the machine size below which Pix offers NO local model
// and points at Ollama Cloud. A product decision that deliberately overrides
// the fits-in-usable-RAM rule: 16 x 0.67 = 10.7 clears the 9B's 10 GB gate on
// paper, but the 9B wires ~9.3 GB beside macOS, a browser, an editor and the
// agent — the probe passes in the machine's one idle moment and the user meets
// the thrash mid-session.
const LocalFloorTotalGB = 24

// LocalOllamaRung is the hand-maintained, setup-only local-hardware fact for
// one Ollama rung pix can offer to pull: RAM, download size, and declared
// context only. It carries no price, no accuracy, no score, and no
// routing/intent binding -- those lived on the (deleted) router's scored
// catalog and never belong here. hardware_shape_test.go pins that boundary
// by reflection so a future edit cannot quietly widen this struct back into
// a routing candidate.
type LocalOllamaRung struct {
	ID            string  // fully qualified "ollama/<tag>"
	Label         string  // human label
	ContextWindow int     // declared RAM-budgeted context, also the num_ctx setup/the bridge send
	MinRAMGB      float64 // ceil(DownloadGB*1.15 + ContextWindow*KVGBPerTok + 1)
	DownloadGB    float64 // on-disk weight size
	KVGBPerTok    float64 // fp16 KV-cache cost per token MinRAMGB was priced with
}

// FitsMemory reports whether m fits a USABLE-memory budget in GB. A rung with
// no declared MinRAMGB never fits: an undeclared requirement is not a small one.
func (m LocalOllamaRung) FitsMemory(usableGB float64) bool {
	if m.MinRAMGB <= 0 {
		return false
	}
	return m.MinRAMGB <= usableGB
}

// localOllamaRungs is the shipped local ladder, LARGEST FIRST (by MinRAMGB, id
// as the deterministic tiebreak): setup offers the largest rung that fits, and
// the setup probe walks the same order. Four rows, hand-maintained here rather
// than sourced from any scored catalog -- see docs/design/ollama-inference.md
// for the RAM formula each row was priced with.
var localOllamaRungs = []LocalOllamaRung{
	{ID: "ollama/qwen3.5:35b", Label: "Qwen 3.5 35B (local)", ContextWindow: 32768, MinRAMGB: 33, DownloadGB: 24.0, KVGBPerTok: 0.0001220703125},
	{ID: "ollama/qwen3.5:27b", Label: "Qwen 3.5 27B (local)", ContextWindow: 32768, MinRAMGB: 24, DownloadGB: 17.0, KVGBPerTok: 9.1552734375e-05},
	{ID: "ollama/qwen3.5:9b", Label: "Qwen 3.5 9B (local)", ContextWindow: 16384, MinRAMGB: 10, DownloadGB: 6.6, KVGBPerTok: 4.57763671875e-05},
	{ID: "ollama/qwen3.5:4b", Label: "Qwen 3.5 4B (local)", ContextWindow: 8192, MinRAMGB: 6, DownloadGB: 3.4, KVGBPerTok: 3.0517578125e-05},
}

// LocalOllamaRungs returns the shipped local ladder, largest MinRAMGB first. A
// fresh copy every call: callers are free to mutate their own slice.
func LocalOllamaRungs() []LocalOllamaRung {
	out := make([]LocalOllamaRung, len(localOllamaRungs))
	copy(out, localOllamaRungs)
	return out
}

// ChooseLocalRung picks the largest local rung whose min_ram_gb fits the probed
// USABLE budget, on a machine big enough to be offered one at all. An OFFER
// filter, never a verdict and never a download. A machine we could not size
// gets NOTHING: unmeasured is likelier small than large, so the floor rung
// would land on exactly the machines the floor protects.
func ChooseLocalRung(mem HostMemory) (LocalOllamaRung, bool) {
	if !mem.OK || mem.TotalGB < LocalFloorTotalGB {
		return LocalOllamaRung{}, false
	}
	for _, m := range localOllamaRungs {
		if m.FitsMemory(mem.UsableGB) {
			return m, true
		}
	}
	return LocalOllamaRung{}, false
}

// LocalRungOfferLine is the one sentence setup prints about the machine it just
// measured: what is offered, and why nothing is when nothing fits.
func LocalRungOfferLine(mem HostMemory, rung LocalOllamaRung, ok bool) string {
	switch {
	case !mem.OK:
		source := mem.Source
		if source == "" {
			source = "unsupported platform"
		}
		return fmt.Sprintf("  could not size this machine (%s failed) — not offering a local model; use Ollama Cloud or an API key", source)
	case mem.TotalGB < LocalFloorTotalGB:
		return fmt.Sprintf("  %.0f GB RAM — below the %d GB Pix needs before a local model is worth running alongside your editor and browser; use Ollama Cloud or an API key",
			mem.TotalGB, LocalFloorTotalGB)
	case !ok:
		return fmt.Sprintf("  %.0f GB RAM (usable ~%.0f GB) does not fit the smallest local model — use Ollama Cloud or an API key instead",
			mem.TotalGB, mem.UsableGB)
	default:
		return fmt.Sprintf("  %.0f GB RAM (usable ~%.0f GB, %s) — offering %s (needs ~%.0f GB usable, %.1f GB download)",
			mem.TotalGB, mem.UsableGB, mem.Source, OllamaTagFor(rung.ID), rung.MinRAMGB, rung.DownloadGB)
	}
}
