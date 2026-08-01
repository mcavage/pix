package main

import (
	"fmt"
	"strings"
	"testing"

	"pix/host/routing"
)

func hwMemEnv(t *testing.T, goos string, totalGB float64) shellEnv {
	t.Helper()
	env := shellEnv{}
	switch goos {
	case "darwin":
		env.run = func(name string, args ...string) (string, error) {
			if name == "sysctl" {
				return fmt.Sprintf("%d\n", int64(totalGB*bytesPerGB)), nil
			}
			return "", fmt.Errorf("unexpected command %s", name)
		}
	case "linux":
		env.readFile = func(path string) (string, error) {
			if path != "/proc/meminfo" {
				return "", fmt.Errorf("unexpected file %s", path)
			}
			return fmt.Sprintf("MemFree:  1024 kB\nMemTotal:       %d kB\nSwapTotal: 0 kB\n", int64(totalGB*bytesPerGB/1024)), nil
		}
	}
	return env
}

// TestUsableMemoryByOS pins the fraction table. Applying the macOS unified-
// memory fraction to Linux (which has no such guarantee) would over-promise on
// every Linux box.
func TestUsableMemoryByOS(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		totalGB float64
		want    float64
		ok      bool
	}{
		{"darwin", 16, 0.67, true},
		{"darwin", 36, 0.67, true},
		{"darwin", 48, 0.75, true},
		{"linux", 8, 0.60, true},
		{"linux", 128, 0.60, true},
		{"windows", 32, 0, false},
		{"plan9", 32, 0, false},
	} {
		got, ok := usableFraction(tc.goos, tc.totalGB)
		if got != tc.want || ok != tc.ok {
			t.Errorf("usableFraction(%q, %g) = %g, %v; want %g, %v", tc.goos, tc.totalGB, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDarwinUsableFractionIsTiered: a single 0.75 over-promises against the
// macOS wired-memory limit on small machines, which is the S1 arithmetic bug.
func TestDarwinUsableFractionIsTiered(t *testing.T) {
	small, _ := usableFraction("darwin", 32)
	large, _ := usableFraction("darwin", 48)
	if small != 0.67 {
		t.Errorf("32 GB darwin fraction = %g, want 0.67", small)
	}
	if large != 0.75 {
		t.Errorf("48 GB darwin fraction = %g, want 0.75", large)
	}
	if small == large {
		t.Fatal("the darwin fraction collapsed back to one number")
	}
}

func TestProbeHostMemoryReadsBothPlatformSeams(t *testing.T) {
	mac := probeHostMemoryFor("darwin", hwMemEnv(t, "darwin", 48))
	if !mac.OK || mac.TotalGB != 48 || mac.Source != "sysctl hw.memsize" {
		t.Fatalf("darwin memory = %+v", mac)
	}
	if mac.UsableGB != 36 {
		t.Fatalf("darwin usable = %g, want 36 (48 * 0.75)", mac.UsableGB)
	}
	lin := probeHostMemoryFor("linux", hwMemEnv(t, "linux", 32))
	if !lin.OK || lin.TotalGB != 32 || lin.Source != "/proc/meminfo MemTotal" {
		t.Fatalf("linux memory = %+v", lin)
	}
	if lin.UsableGB != 32*0.60 {
		t.Fatalf("linux usable = %g, want 19.2", lin.UsableGB)
	}
	if got := probeHostMemoryFor("darwin", shellEnv{}); got.OK {
		t.Fatalf("an unwired seam must not report a size: %+v", got)
	}
	if got := probeHostMemoryFor("windows", hwMemEnv(t, "linux", 32)); got.OK {
		t.Fatalf("an unsupported GOOS must not be sized: %+v", got)
	}
}

// TestChooseLocalRungByRAM is the D2 table. The headline correction it guards:
// a 32 GB Mac is offered the 9b, NOT the 27b — rev 1's arithmetic (weights
// only, no KV term) offered the 27b to exactly the machine that thrashes on it.
func TestChooseLocalRungByRAM(t *testing.T) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		goos    string
		totalGB float64
		want    string // "" = nothing on the ladder fits
	}{
		{"darwin", 8, ""},
		{"linux", 8, ""},
		// NOTE: the design's table prints 4b for a 16 GB Mac, but 16 * 0.67 =
		// 10.72 >= the 9b's 10 GB gate, and the stated rule is "the largest rung
		// whose min_ram_gb <= usable". The rule wins; the table row is a slip.
		{"darwin", 16, "ollama/qwen3.5:9b"},
		{"linux", 16, "ollama/qwen3.5:4b"},
		{"darwin", 24, "ollama/qwen3.5:9b"},
		{"linux", 24, "ollama/qwen3.5:9b"},
		{"darwin", 32, "ollama/qwen3.5:9b"},
		{"linux", 32, "ollama/qwen3.5:9b"},
		{"darwin", 36, "ollama/qwen3.5:27b"},
		{"linux", 36, "ollama/qwen3.5:9b"},
		{"darwin", 48, "ollama/qwen3.5:35b"},
		{"linux", 48, "ollama/qwen3.5:27b"},
		{"darwin", 64, "ollama/qwen3.5:35b"},
		{"linux", 64, "ollama/qwen3.5:35b"},
		{"darwin", 128, "ollama/qwen3.5:35b"},
		{"linux", 128, "ollama/qwen3.5:35b"},
	} {
		mem := probeHostMemoryFor(tc.goos, hwMemEnv(t, tc.goos, tc.totalGB))
		if !mem.OK {
			t.Fatalf("%s %gGB: probe failed: %+v", tc.goos, tc.totalGB, mem)
		}
		rung, ok := chooseLocalRung(reg, mem)
		switch {
		case tc.want == "" && ok:
			t.Errorf("%s %gGB (usable %.1f): offered %s, want nothing", tc.goos, tc.totalGB, mem.UsableGB, rung.ID)
		case tc.want != "" && !ok:
			t.Errorf("%s %gGB (usable %.1f): offered nothing, want %s", tc.goos, tc.totalGB, mem.UsableGB, tc.want)
		case tc.want != "" && rung.ID != tc.want:
			t.Errorf("%s %gGB (usable %.1f): offered %s, want %s", tc.goos, tc.totalGB, mem.UsableGB, rung.ID, tc.want)
		}
	}
}

// TestUnknownMemoryOffersFloorRungOnly: "unknown" never means "unconstrained".
func TestUnknownMemoryOffersFloorRungOnly(t *testing.T) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rung, ok := chooseLocalRung(reg, hostMemory{Source: "sysctl hw.memsize"})
	if !ok {
		t.Fatal("an unsized machine still gets the floor rung offered (the pull remains consented)")
	}
	if rung.ID != "ollama/qwen3.5:4b" {
		t.Fatalf("unsized machine was offered %s, want the floor rung ollama/qwen3.5:4b", rung.ID)
	}
	line := localRungOfferLine(hostMemory{Source: "sysctl hw.memsize"}, rung, true)
	if !strings.Contains(line, "could not size this machine") {
		t.Fatalf("the offer line must say the machine was not sized, got %q", line)
	}
}

// TestHardwareCheckIsNeverReady is invariant 13 in this file's own terms: a RAM
// reading is an inference, and an inference can never produce a verdict.
func TestHardwareCheckIsNeverReady(t *testing.T) {
	for _, mem := range []hostMemory{
		{},
		{Source: "sysctl hw.memsize"},
		{TotalGB: 8, UsableGB: 5.4, Source: "sysctl hw.memsize", OK: true},
		{TotalGB: 128, UsableGB: 96, Source: "sysctl hw.memsize", OK: true},
		{TotalGB: 32, UsableGB: 19.2, Source: "/proc/meminfo MemTotal", OK: true},
	} {
		for _, c := range hardwareCheck(mem) {
			if c.verdict == verdictReady {
				t.Errorf("hardwareCheck(%+v) rendered ready; a hardware reading is not a probe", mem)
			}
			if !c.note {
				t.Errorf("hardwareCheck(%+v) is not a note; it must never block or count as outstanding", mem)
			}
			if c.todo != "" {
				t.Errorf("hardwareCheck(%+v) offered a fix command (%q); RAM is not a configuration mistake", mem, c.todo)
			}
		}
	}
}

// TestMinRAMArithmeticMatchesTheShippedCatalog keeps the host-side helper and
// the catalog honest about the same formula.
func TestMinRAMArithmeticMatchesTheShippedCatalog(t *testing.T) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range routing.LocalRungs(reg) {
		if got := minRAMFor(m); got != m.MinRAMGB {
			t.Errorf("%s: catalog min_ram_gb %g, recomputed %g", m.ID, m.MinRAMGB, got)
		}
	}
}
