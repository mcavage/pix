package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

func TestParseLatestReleaseLocation(t *testing.T) {
	ok := map[string]string{
		"https://github.com/mcavage/pix/releases/tag/v0.1.2":   "0.1.2",
		"https://github.com/mcavage/pix/releases/tag/0.1.2":    "0.1.2",
		"https://github.com/mcavage/pix/releases/tag/v10.20.3": "10.20.3",
		"https://github.com/mcavage/pix/releases/tag/v0.1.2?x": "0.1.2",
		"https://github.com/mcavage/pix/releases/tag/v0.1.2#a": "0.1.2",
	}
	for loc, want := range ok {
		if got := parseLatestReleaseLocation(loc); got != want {
			t.Errorf("parseLatestReleaseLocation(%q) = %q, want %q", loc, got, want)
		}
	}
	// Anything that is not a clean released semver must yield "" — a bogus pin is
	// strictly worse than falling back to the stamped version.
	bad := []string{
		"",
		"https://github.com/mcavage/pix/releases",
		"https://github.com/login?return_to=%2Fmcavage%2Fpix",
		"https://github.com/mcavage/pix/releases/tag/",
		"https://github.com/mcavage/pix/releases/tag/nightly",
		"https://github.com/mcavage/pix/releases/tag/v0.1",
		"https://github.com/mcavage/pix/releases/tag/v0.1.2-rc1",
		"https://github.com/mcavage/pix/releases/tag/v0.1.2+local",
	}
	for _, loc := range bad {
		if got := parseLatestReleaseLocation(loc); got != "" {
			t.Errorf("parseLatestReleaseLocation(%q) = %q, want \"\"", loc, got)
		}
	}
}

func TestResolveKitRef_Precedence(t *testing.T) {
	cases := []struct {
		name                    string
		version, flag, pin, ref string
		src                     kitRefSource
	}{
		{"flag beats everything", "0.1.0", "main", "0.9.9", "main", kitRefFlag},
		{"config pin becomes a tag", "0.1.0", "", "0.1.2", "v0.1.2", kitRefConfigPin},
		{"config pin accepts a v prefix", "0.1.0", "", "v0.1.0", "v0.1.0", kitRefConfigPin},
		{"config pin passes a branch through", "0.1.0", "", "main", "main", kitRefConfigPin},
		{"released default stays stamped", "0.1.2", "", "", "", kitRefStamped},
		{"unreleased build uses local handling", "0.1.0+local", "", "", "", kitRefUnreleased},
		{"dev build uses local handling", "dev", "", "", "", kitRefUnreleased},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, src := resolveKitRef(c.version, c.flag, c.pin)
			if ref != c.ref || src != c.src {
				t.Errorf("resolveKitRef(%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.version, c.flag, c.pin, ref, src, c.ref, c.src)
			}
		})
	}
}

func TestResolveKitRef_UnreleasedUsesLocalHandling(t *testing.T) {
	for _, v := range []string{"dev", "0.1.0+local", "", "not-semver", "0.1"} {
		if ref, src := resolveKitRef(v, "", ""); ref != "" || src != kitRefUnreleased {
			t.Errorf("version %q: got (%q,%v), want (\"\",kitRefUnreleased)", v, ref, src)
		}
	}
}

func TestFetchLatestRelease_FollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/mcavage/pix/releases/tag/v0.1.2", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if got := fetchLatestRelease(srv.Client(), srv.URL+"/mcavage/pix/releases/latest"); got != "0.1.2" {
		t.Errorf("fetchLatestRelease = %q, want 0.1.2", got)
	}
}

// Every network failure mode must degrade to "" so the caller keeps the stamped
// pin. A resolver that can fail a run is worse than no resolver.
func TestFetchLatestRelease_FailuresYieldEmpty(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer down.Close()
	if got := fetchLatestRelease(down.Client(), down.URL); got != "" {
		t.Errorf("500 response = %q, want \"\"", got)
	}

	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer login.Close()
	if got := fetchLatestRelease(login.Client(), login.URL); got != "" {
		t.Errorf("login redirect = %q, want \"\"", got)
	}

	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := unreachable.URL
	unreachable.Close() // nothing is listening now
	if got := fetchLatestRelease(&http.Client{Timeout: time.Second}, url); got != "" {
		t.Errorf("unreachable host = %q, want \"\"", got)
	}

	if got := fetchLatestRelease(&http.Client{}, "://not a url"); got != "" {
		t.Errorf("malformed url = %q, want \"\"", got)
	}
}

func TestKitRefNotice(t *testing.T) {
	if msg := kitRefNotice("0.1.0", "v0.1.0", kitRefConfigPin); !strings.Contains(msg, "version_pin") {
		t.Errorf("config-pin notice should name version_pin, got %q", msg)
	}
	// The user's own --kit-ref needs no echo, and the default is not news.
	if msg := kitRefNotice("0.1.0", "main", kitRefFlag); msg != "" {
		t.Errorf("explicit --kit-ref should be silent, got %q", msg)
	}
	if msg := kitRefNotice("0.1.0", "", kitRefStamped); msg != "" {
		t.Errorf("stamped default should be silent, got %q", msg)
	}
}

// The resolved ref must actually reach the composed sbx argv.
func TestBuildSbxArgs_KitRefOverridesStampedPin(t *testing.T) {
	args := buildSbxArgs(&config.Config{}, runOpts{Workspace: ".", KitRef: "v0.1.2"}, "0.1.0")
	want := "git+https://github.com/mcavage/pix.git#ref=v0.1.2&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("expected kit %q in %v", want, args)
	}
	// Empty KitRef keeps the original lockstep behaviour exactly.
	args = buildSbxArgs(&config.Config{}, runOpts{Workspace: "."}, "0.1.0")
	stamped := "git+https://github.com/mcavage/pix.git#ref=v0.1.0&dir=pi-kit"
	if !contains(args, []string{"--kit", stamped}) {
		t.Errorf("expected stamped kit %q in %v", stamped, args)
	}
	// An explicit --kit still replaces the auto pin outright.
	args = buildSbxArgs(&config.Config{}, runOpts{Workspace: ".", KitRef: "v0.1.2", Kits: []string{"/local/pi-kit"}}, "0.1.0")
	for _, a := range args {
		if strings.Contains(a, "#ref=") {
			t.Errorf("--kit override must suppress the auto pin, got %q", a)
		}
	}
}

func TestParseRunArgs_KitRef(t *testing.T) {
	o, err := parseRunArgs([]string{"--kit-ref", "0.1.2"})
	if err != nil {
		t.Fatal(err)
	}
	if o.KitRef != "v0.1.2" {
		t.Errorf("--kit-ref 0.1.2 = %q, want v0.1.2 (a bare semver means that tag)", o.KitRef)
	}
	if o, err = parseRunArgs([]string{"--kit-ref=main"}); err != nil || o.KitRef != "main" {
		t.Errorf("--kit-ref=main = (%q,%v), want (main,nil)", o.KitRef, err)
	}
}
