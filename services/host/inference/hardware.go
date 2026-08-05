package inference

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"

	"pix/host/hostenv"
	"pix/host/routing"
	"pix/host/sys"
)

// hardware.go owns ONE fact — how much physical memory this machine has — and
// it exists for exactly one purpose: sizing the LOCAL model this host can be
// OFFERED. It is part of the inference domain for that reason, and it inherits
// one rule verbatim:
//
//	AN INFERENCE INFORMS REMEDIATION AND OFFERS. IT CAN NEVER PRODUCE A VERDICT.
//
// A RAM reading proves nothing is installed, configured or callable, so it may
// size an offer and explain why a rung was not offered, and it may never make
// anything read as ready (safety invariant 13 — success words are earned by a
// probe).
//
// The reading is deliberately of TOTAL physical memory, not free/available:
// free memory is a snapshot of an unrelated moment, and a machine with a
// browser open would be offered a smaller model forever.

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

// darwinFractionTierGB is the total-RAM inflection where macOS's default GPU
// wired-memory limit (`sysctl iogpu.wired_limit_mb`, 0 = system default) moves
// from roughly two thirds of RAM to roughly three quarters. Apple has never
// contracted it, so it is re-grounded like a scorecard number, not derived.
const darwinFractionTierGB = 36

// UsableFraction is the share of TOTAL RAM a model runtime may plan on, by OS
// and (on darwin) by machine size. The darwin number is TWO-TIER because the
// default Metal working-set ceiling is: a flat 0.75 over-promises on exactly
// the small unified-memory machines that can least afford it, and the runtime
// then spills to the CPU path or swaps.
func UsableFraction(goos string, totalGB float64) (float64, bool) {
	switch goos {
	case "darwin":
		if totalGB > darwinFractionTierGB {
			return 0.75, true
		}
		return 0.67, true
	case "linux":
		// No unified-memory guarantee, discrete VRAM is not probed in v1, and
		// CPU inference contends with the desktop. Deliberately conservative.
		return 0.60, true
	default:
		return 0, false
	}
}

// ProbeHostMemory reads TOTAL physical memory through the hostenv.Env seams, so
// it is fakeable in tests and never links cgo. darwin: `sysctl -n hw.memsize`
// (bytes). linux: /proc/meminfo MemTotal (kB). Any other GOOS: OK=false.
func ProbeHostMemory(env hostenv.Env) HostMemory {
	return ProbeHostMemoryFor(runtime.GOOS, env)
}

// ProbeHostMemoryFor is ProbeHostMemory with the OS injected, which is the only
// way a hermetic test can exercise both readers on one machine.
func ProbeHostMemoryFor(goos string, env hostenv.Env) HostMemory {
	var totalGB float64
	var source string
	switch goos {
	case "darwin":
		out, ok := probeMemoryCommand(env, "sysctl", "-n", "hw.memsize")
		if !ok {
			return HostMemory{Source: "sysctl hw.memsize"}
		}
		bytes, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if err != nil || bytes <= 0 {
			return HostMemory{Source: "sysctl hw.memsize"}
		}
		totalGB, source = bytes/BytesPerGB, "sysctl hw.memsize"
	case "linux":
		body, err := env.ReadFile("/proc/meminfo")
		if err != nil {
			return HostMemory{Source: "/proc/meminfo MemTotal"}
		}
		kb, ok := parseMemTotalKB(body)
		if !ok {
			return HostMemory{Source: "/proc/meminfo MemTotal"}
		}
		totalGB, source = kb*1024/BytesPerGB, "/proc/meminfo MemTotal"
	default:
		return HostMemory{}
	}
	fraction, ok := UsableFraction(goos, totalGB)
	if !ok {
		return HostMemory{Source: source}
	}
	return HostMemory{TotalGB: totalGB, UsableGB: totalGB * fraction, Source: source, OK: true}
}

// probeMemoryCommand runs a hardware probe under the bounded seam, so a wedged
// sysctl can never hang setup.
func probeMemoryCommand(env sys.Exec, name string, args ...string) (string, bool) {
	out, timedOut, err := env.RunTimed(name, args...)
	return out, err == nil && !timedOut
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
// and points at Ollama Cloud instead. It is a product decision, not arithmetic,
// and it deliberately overrides the fits-in-usable-RAM rule below it.
//
// That rule alone said a 16 GB Mac should run the 9B: 16 x 0.67 = 10.7, over
// its 10 GB gate. True only of an idle machine. The 9B wires ~9.3 GB, leaving
// ~6.7 GB for macOS, a browser, an editor and the agent itself, and the setup
// probe passes anyway because it runs during the one idle moment the machine
// ever has — so the user meets the thrash mid-session, when a probe can no
// longer save them. Below this floor the honest answer is Ollama Cloud, not a
// model small enough to fit but too small to code with.
const LocalFloorTotalGB = 24

// ChooseLocalRung picks the largest local rung whose min_ram_gb fits the probed
// USABLE budget, on a machine big enough to be offered one at all. It is an
// OFFER filter, never a verdict and never a download.
//
// A machine we could not size gets NOTHING, not the floor rung: an earlier
// draft offered the smallest model on an unmeasured box, but combined with the
// 24 GB floor that would hand a local model to precisely the machines the floor
// exists to protect, since an unmeasured machine is more likely small than
// large. Unknown size means Cloud, and the offer line says why.
func ChooseLocalRung(reg *routing.Registry, mem HostMemory) (routing.Model, bool) {
	rungs := routing.LocalRungs(reg) // largest first
	if len(rungs) == 0 || !mem.OK || mem.TotalGB < LocalFloorTotalGB {
		return routing.Model{}, false
	}
	for _, m := range rungs {
		if m.FitsMemory(mem.UsableGB) {
			return m, true
		}
	}
	return routing.Model{}, false
}

// LocalRungOfferLine is the one honest sentence setup prints about the machine
// it just measured. It never claims a model works — only what is offered, and
// why nothing is when nothing fits.
func LocalRungOfferLine(mem HostMemory, rung routing.Model, ok bool) string {
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

// MinRAMFor recomputes a rung's gate from its own declared terms. Nothing reads
// it at runtime — it exists so a test can hold the catalog to the arithmetic
// the design fixed (weights*1.15 + declared context * KV per token + 1).
func MinRAMFor(m routing.Model) float64 {
	return math.Ceil(m.DownloadGB*1.15 + float64(m.ContextWindow)*m.KVGBPerTok + 1.0)
}
