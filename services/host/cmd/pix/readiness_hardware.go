package main

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"

	"pix/host/routing"
)

// readiness_hardware.go owns ONE hardware fact — how much physical memory this
// machine has — and it inherits readiness_ollama.go's header rule verbatim:
//
//	AN INFERENCE INFORMS REMEDIATION AND OFFERS. IT CAN NEVER PRODUCE A VERDICT.
//
// A RAM reading is not a probe of anything Pix ships: it proves nothing is
// installed, configured, or callable. So it may size the local models setup
// OFFERS, and it may explain why a rung was not offered, but it may never
// render `ready` (safety invariant 13 — success words are earned by a probe).
// hardwareCheck is therefore always a note and never green.
//
// The reading is deliberately of TOTAL physical memory, not free/available
// memory: free memory is a snapshot of an unrelated moment, and a machine with
// a browser open would be offered a smaller model forever.

// hostMemory is the probed physical-memory fact, the fraction of it a model
// runtime may plan on, and where the number came from. OK=false means the
// machine could not be sized: callers degrade to the floor rung, never up.
type hostMemory struct {
	TotalGB  float64
	UsableGB float64
	Source   string // "sysctl hw.memsize" | "/proc/meminfo MemTotal" | ""
	OK       bool
}

// bytesPerGB is 2^30 — the unit every number in the ladder is expressed in.
const bytesPerGB = 1 << 30

// darwinFractionTierGB is the total-RAM inflection where macOS's default GPU
// wired-memory limit (`sysctl iogpu.wired_limit_mb`, 0 = system default) moves
// from roughly two thirds of RAM to roughly three quarters. Apple has never
// contracted it, so it is re-grounded like a scorecard number, not derived.
const darwinFractionTierGB = 36

// usableFraction is the share of TOTAL RAM a model runtime may plan on, by OS
// and (on darwin) by machine size. The darwin number is TWO-TIER because the
// default Metal working-set ceiling is: a flat 0.75 over-promises on exactly
// the small unified-memory machines that can least afford it, and the runtime
// then spills to the CPU path or swaps.
//
// Deviation from the design's `usableFraction(goos string)` signature: the
// two-tier darwin rule cannot be expressed without the machine's size.
func usableFraction(goos string, totalGB float64) (float64, bool) {
	switch goos {
	case "darwin":
		if totalGB > darwinFractionTierGB {
			return 0.75, true
		}
		return 0.67, true
	case "linux":
		// No unified-memory guarantee, discrete VRAM is not probed in v1, and CPU
		// inference contends with the desktop. Deliberately conservative.
		return 0.60, true
	default:
		return 0, false
	}
}

// probeHostMemory reads TOTAL physical memory through the shellEnv seams, so it
// is fakeable in tests and never links cgo. darwin: `sysctl -n hw.memsize`
// (bytes). linux: /proc/meminfo MemTotal (kB). Any other GOOS: OK=false.
func probeHostMemory(env shellEnv) hostMemory {
	return probeHostMemoryFor(runtime.GOOS, env)
}

// probeHostMemoryFor is probeHostMemory with the OS injected, which is the only
// way a hermetic test can exercise both readers on one machine.
func probeHostMemoryFor(goos string, env shellEnv) hostMemory {
	var totalGB float64
	var source string
	switch goos {
	case "darwin":
		out, ok := probeMemoryCommand(env, "sysctl", "-n", "hw.memsize")
		if !ok {
			return hostMemory{Source: "sysctl hw.memsize"}
		}
		bytes, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if err != nil || bytes <= 0 {
			return hostMemory{Source: "sysctl hw.memsize"}
		}
		totalGB, source = bytes/bytesPerGB, "sysctl hw.memsize"
	case "linux":
		if env.readFile == nil {
			return hostMemory{Source: "/proc/meminfo MemTotal"}
		}
		body, err := env.readFile("/proc/meminfo")
		if err != nil {
			return hostMemory{Source: "/proc/meminfo MemTotal"}
		}
		kb, ok := parseMemTotalKB(body)
		if !ok {
			return hostMemory{Source: "/proc/meminfo MemTotal"}
		}
		totalGB, source = kb*1024/bytesPerGB, "/proc/meminfo MemTotal"
	default:
		return hostMemory{}
	}
	fraction, ok := usableFraction(goos, totalGB)
	if !ok {
		return hostMemory{Source: source}
	}
	return hostMemory{TotalGB: totalGB, UsableGB: totalGB * fraction, Source: source, OK: true}
}

// probeMemoryCommand prefers the BOUNDED probe seam so a wedged sysctl can
// never hang setup, falling back to the plain runner tests usually wire.
func probeMemoryCommand(env shellEnv, name string, args ...string) (string, bool) {
	if env.probe != nil {
		out, timedOut, err := env.probe(name, args...)
		return out, err == nil && !timedOut
	}
	if env.run != nil {
		out, err := env.run(name, args...)
		return out, err == nil
	}
	return "", false
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

// localFloorTotalGB is the machine size below which Pix offers NO local model
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
const localFloorTotalGB = 24

// chooseLocalRung picks the largest local rung whose min_ram_gb fits the probed
// USABLE budget, on a machine big enough to be offered one at all. It is an
// OFFER filter, never a verdict and never a download.
//
// A machine we could not size gets NOTHING, not the floor rung: an earlier
// draft offered the smallest model on an unmeasured box, but combined with the
// 24 GB floor that would hand a local model to precisely the machines the floor
// exists to protect, since an unmeasured machine is more likely small than
// large. Unknown size means Cloud, and the offer line says why.
func chooseLocalRung(reg *routing.Registry, mem hostMemory) (routing.Model, bool) {
	rungs := routing.LocalRungs(reg) // largest first
	if len(rungs) == 0 || !mem.OK || mem.TotalGB < localFloorTotalGB {
		return routing.Model{}, false
	}
	for _, m := range rungs {
		if m.FitsMemory(mem.UsableGB) {
			return m, true
		}
	}
	return routing.Model{}, false
}

// localRungOfferLine is the one honest sentence setup prints about the machine
// it just measured. It never claims a model works — only what is offered, and
// why nothing is when nothing fits.
func localRungOfferLine(mem hostMemory, rung routing.Model, ok bool) string {
	switch {
	case !mem.OK:
		source := mem.Source
		if source == "" {
			source = "unsupported platform"
		}
		return fmt.Sprintf("  could not size this machine (%s failed) — not offering a local model; use Ollama Cloud or an API key", source)
	case mem.TotalGB < localFloorTotalGB:
		return fmt.Sprintf("  %.0f GB RAM — below the %d GB Pix needs before a local model is worth running alongside your editor and browser; use Ollama Cloud or an API key",
			mem.TotalGB, localFloorTotalGB)
	case !ok:
		return fmt.Sprintf("  %.0f GB RAM (usable ~%.0f GB) does not fit the smallest local model — use Ollama Cloud or an API key instead",
			mem.TotalGB, mem.UsableGB)
	default:
		return fmt.Sprintf("  %.0f GB RAM (usable ~%.0f GB, %s) — offering %s (needs ~%.0f GB usable, %.1f GB download)",
			mem.TotalGB, mem.UsableGB, mem.Source, ollamaTagFor(rung.ID), rung.MinRAMGB, rung.DownloadGB)
	}
}

// hardwareCheck renders the doctor row. It is ALWAYS a note and NEVER ready:
// see this file's header. There is nothing to fix, so it is never a todo
// either — RAM is not a configuration mistake.
func hardwareCheck(mem hostMemory) []check {
	c := check{label: "hardware", note: true, verdict: verdictUnverifiable}
	if !mem.OK {
		source := mem.Source
		if source == "" {
			source = "unsupported platform"
		}
		c.detail = "could not size this machine (" + source + ") — local model offers degrade to the smallest rung; not a readiness verdict"
		c.evidence = "host memory unreadable via " + source
		return []check{c}
	}
	c.detail = fmt.Sprintf("%.0f GB (usable ~%.0f GB, %s) — informs local model offers; not a readiness verdict",
		mem.TotalGB, mem.UsableGB, mem.Source)
	c.evidence = fmt.Sprintf("%s: %.0f GB total, planning on %.0f GB", mem.Source, mem.TotalGB, mem.UsableGB)
	return []check{c}
}

// minRAMFor recomputes a rung's gate from its own declared terms. Nothing reads
// it at runtime — it exists so a test can hold the catalog to the arithmetic
// the design fixed (weights*1.15 + declared context * KV per token + 1).
func minRAMFor(m routing.Model) float64 {
	return math.Ceil(m.DownloadGB*1.15 + float64(m.ContextWindow)*m.KVGBPerTok + 1.0)
}

// ollamaTagFor strips the catalog's provider prefix, giving the tag `ollama
// pull` and `ollama list` actually speak.
func ollamaTagFor(catalogID string) string {
	return strings.TrimPrefix(catalogID, "ollama/")
}
