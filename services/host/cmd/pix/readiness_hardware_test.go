package main

import (
	"fmt"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/routing"
	"pix/host/sys/systest"
)

func hwMemEnv(t *testing.T, goos string, totalGB float64) hostenv.Env {
	t.Helper()
	env := hostenv.Env{System: &systest.Fake{}}
	switch goos {
	case "darwin":
		systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
			if name == "sysctl" {
				return fmt.Sprintf("%d\n", int64(totalGB*axis.BytesPerGB)), nil
			}
			return "", fmt.Errorf("unexpected command %s", name)
		}
	case "linux":
		systest.Of(env.System).ReadFileFn = func(path string) (string, error) {
			if path != "/proc/meminfo" {
				return "", fmt.Errorf("unexpected file %s", path)
			}
			return fmt.Sprintf("MemFree:  1024 kB\nMemTotal:       %d kB\nSwapTotal: 0 kB\n", int64(totalGB*axis.BytesPerGB/1024)), nil
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
		got, ok := axis.UsableFraction(tc.goos, tc.totalGB)
		if got != tc.want || ok != tc.ok {
			t.Errorf("axis.UsableFraction(%q, %g) = %g, %v; want %g, %v", tc.goos, tc.totalGB, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDarwinUsableFractionIsTiered: a single 0.75 over-promises against the
// macOS wired-memory limit on small machines, which is the S1 arithmetic bug.
func TestDarwinUsableFractionIsTiered(t *testing.T) {
	small, _ := axis.UsableFraction("darwin", 32)
	large, _ := axis.UsableFraction("darwin", 48)
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
	mac := axis.ProbeHostMemoryFor("darwin", hwMemEnv(t, "darwin", 48))
	if !mac.OK || mac.TotalGB != 48 || mac.Source != "sysctl hw.memsize" {
		t.Fatalf("darwin memory = %+v", mac)
	}
	if mac.UsableGB != 36 {
		t.Fatalf("darwin usable = %g, want 36 (48 * 0.75)", mac.UsableGB)
	}
	lin := axis.ProbeHostMemoryFor("linux", hwMemEnv(t, "linux", 32))
	if !lin.OK || lin.TotalGB != 32 || lin.Source != "/proc/meminfo MemTotal" {
		t.Fatalf("linux memory = %+v", lin)
	}
	if lin.UsableGB != 32*0.60 {
		t.Fatalf("linux usable = %g, want 19.2", lin.UsableGB)
	}
	if got := axis.ProbeHostMemoryFor("darwin", hostenv.Env{System: &systest.Fake{}}); got.OK {
		t.Fatalf("an unwired seam must not report a size: %+v", got)
	}
	if got := axis.ProbeHostMemoryFor("windows", hwMemEnv(t, "linux", 32)); got.OK {
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
		// Below axis.LocalFloorTotalGB (24) NOTHING is offered, whatever the
		// usable-budget arithmetic says. A 16 GB Mac clears the 9b's 10 GB gate on
		// paper (16 * 0.67 = 10.72) and still should not be handed a local model:
		// the 9b wires ~9.3 GB, and the machine also runs macOS, a browser, an
		// editor and the agent. The floor is a product decision that outranks the
		// arithmetic, and these rows are what pin it.
		{"darwin", 8, ""},
		{"linux", 8, ""},
		{"darwin", 16, ""},
		{"linux", 16, ""},
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
		mem := axis.ProbeHostMemoryFor(tc.goos, hwMemEnv(t, tc.goos, tc.totalGB))
		if !mem.OK {
			t.Fatalf("%s %gGB: probe failed: %+v", tc.goos, tc.totalGB, mem)
		}
		rung, ok := axis.ChooseLocalRung(reg, mem)
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

// TestUnknownMemoryOffersNothing: "unknown" never means "unconstrained", and
// with a 24 GB floor it cannot mean "the floor rung" either. An earlier draft
// offered the smallest model on an unmeasured box; combined with the floor that
// hands a local model to exactly the machines the floor exists to protect,
// since an unmeasured machine is likelier small than large.
func TestUnknownMemoryOffersNothing(t *testing.T) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rung, ok := axis.ChooseLocalRung(reg, axis.HostMemory{Source: "sysctl hw.memsize"})
	if ok {
		t.Fatalf("an unsized machine was offered %s; unknown size must mean no local offer", rung.ID)
	}
	line := axis.LocalRungOfferLine(axis.HostMemory{Source: "sysctl hw.memsize"}, rung, false)
	if !strings.Contains(line, "could not size this machine") {
		t.Fatalf("the offer line must say the machine was not sized, got %q", line)
	}
}

// TestHardwareCheckIsNeverReady is invariant 13 in this file's own terms: a RAM
// reading is an inference, and an inference can never produce a verdict.
func TestHardwareCheckIsNeverReady(t *testing.T) {
	for _, mem := range []axis.HostMemory{
		{},
		{Source: "sysctl hw.memsize"},
		{TotalGB: 8, UsableGB: 5.4, Source: "sysctl hw.memsize", OK: true},
		{TotalGB: 128, UsableGB: 96, Source: "sysctl hw.memsize", OK: true},
		{TotalGB: 32, UsableGB: 19.2, Source: "/proc/meminfo MemTotal", OK: true},
	} {
		for _, c := range axis.HardwareCheck(mem) {
			if c.Verdict == readiness.VerdictReady {
				t.Errorf("axis.HardwareCheck(%+v) rendered ready; a hardware reading is not a probe", mem)
			}
			if !c.Note {
				t.Errorf("axis.HardwareCheck(%+v) is not a note; it must never block or count as outstanding", mem)
			}
			if c.Todo != "" {
				t.Errorf("axis.HardwareCheck(%+v) offered a fix command (%q); RAM is not a configuration mistake", mem, c.Todo)
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
		if got := axis.MinRAMFor(m); got != m.MinRAMGB {
			t.Errorf("%s: catalog min_ram_gb %g, recomputed %g", m.ID, m.MinRAMGB, got)
		}
	}
}
