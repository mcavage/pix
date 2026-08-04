package main

import (
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
)

func TestResolveKitRef_Precedence(t *testing.T) {
	cases := []struct {
		name                    string
		version, flag, pin, ref string
		src                     launch.KitRefSource
	}{
		{"flag beats everything", "0.1.0", "main", "0.9.9", "main", launch.KitRefFlag},
		{"config pin becomes a tag", "0.1.0", "", "0.1.2", "v0.1.2", launch.KitRefConfigPin},
		{"config pin accepts a v prefix", "0.1.0", "", "v0.1.0", "v0.1.0", launch.KitRefConfigPin},
		{"config pin passes a branch through", "0.1.0", "", "main", "main", launch.KitRefConfigPin},
		{"released default stays stamped", "0.1.2", "", "", "", launch.KitRefStamped},
		{"unreleased build uses local handling", "0.1.0+local", "", "", "", launch.KitRefUnreleased},
		{"dev build uses local handling", "dev", "", "", "", launch.KitRefUnreleased},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, src := launch.ResolveKitRef(c.version, c.flag, c.pin)
			if ref != c.ref || src != c.src {
				t.Errorf("launch.ResolveKitRef(%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.version, c.flag, c.pin, ref, src, c.ref, c.src)
			}
		})
	}
}

func TestResolveKitRef_UnreleasedUsesLocalHandling(t *testing.T) {
	for _, v := range []string{"dev", "0.1.0+local", "", "not-semver", "0.1"} {
		if ref, src := launch.ResolveKitRef(v, "", ""); ref != "" || src != launch.KitRefUnreleased {
			t.Errorf("version %q: got (%q,%v), want (\"\",launch.KitRefUnreleased)", v, ref, src)
		}
	}
}

func TestKitRefNotice(t *testing.T) {
	if msg := launch.KitRefNotice("0.1.0", "v0.1.0", launch.KitRefConfigPin); !strings.Contains(msg, "version_pin") {
		t.Errorf("config-pin notice should name version_pin, got %q", msg)
	}
	// The user's own --kit-ref needs no echo, and the default is not news.
	if msg := launch.KitRefNotice("0.1.0", "main", launch.KitRefFlag); msg != "" {
		t.Errorf("explicit --kit-ref should be silent, got %q", msg)
	}
	if msg := launch.KitRefNotice("0.1.0", "", launch.KitRefStamped); msg != "" {
		t.Errorf("stamped default should be silent, got %q", msg)
	}
}

// The resolved ref must actually reach the composed sbx argv.
func TestBuildSbxArgs_KitRefOverridesStampedPin(t *testing.T) {
	args := launch.BuildSbxArgs(&config.Config{}, launch.RunOpts{Workspace: ".", KitRef: "v0.1.2"}, "0.1.0")
	want := "git+https://github.com/mcavage/pix.git#ref=v0.1.2&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("expected kit %q in %v", want, args)
	}
	// Empty KitRef keeps the original lockstep behaviour exactly.
	args = launch.BuildSbxArgs(&config.Config{}, launch.RunOpts{Workspace: "."}, "0.1.0")
	stamped := "git+https://github.com/mcavage/pix.git#ref=v0.1.0&dir=pi-kit"
	if !contains(args, []string{"--kit", stamped}) {
		t.Errorf("expected stamped kit %q in %v", stamped, args)
	}
	// An explicit --kit still replaces the auto pin outright.
	args = launch.BuildSbxArgs(&config.Config{}, launch.RunOpts{Workspace: ".", KitRef: "v0.1.2", Kits: []string{"/local/pi-kit"}}, "0.1.0")
	for _, a := range args {
		if strings.Contains(a, "#ref=") {
			t.Errorf("--kit override must suppress the auto pin, got %q", a)
		}
	}
}

func TestParseRunArgs_KitRef(t *testing.T) {
	o, err := launch.ParseRunArgs([]string{"--kit-ref", "0.1.2"})
	if err != nil {
		t.Fatal(err)
	}
	if o.KitRef != "v0.1.2" {
		t.Errorf("--kit-ref 0.1.2 = %q, want v0.1.2 (a bare semver means that tag)", o.KitRef)
	}
	if o, err = launch.ParseRunArgs([]string{"--kit-ref=main"}); err != nil || o.KitRef != "main" {
		t.Errorf("--kit-ref=main = (%q,%v), want (main,nil)", o.KitRef, err)
	}
}
