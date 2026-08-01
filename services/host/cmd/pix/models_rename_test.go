package main

// models_rename_test.go covers the `route` -> `models` verb rename
// (docs/design/models-cli.md): the deprecation alias still dispatches and
// stays off stdout, and the old spelling never regresses into production
// guidance.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRouteAliasStillDispatches proves `pix route show` reaches the exact same
// handler as `pix models show` (both end up calling `pix-host route show`)
// and that the one-release deprecation notice prints to STDERR ONLY, never
// stdout — a `--json`/piped caller must see nothing but the real payload.
// Runs main() in a subprocess (like the other os.Exit-adjacent tests in this
// package) with a fake `pix-host` on PATH so no real sbx/host state is needed.
func TestRouteAliasStillDispatches(t *testing.T) {
	if os.Getenv("PIX_ROUTE_ALIAS_TEST") == "1" {
		os.Args = []string{"pix", "route", "show", "--json"}
		main()
		return
	}

	dir := t.TempDir()
	fakeHost := filepath.Join(dir, "pix-host")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo dev; exit 0; fi\necho \"ARGV:$@\"\n"
	if err := os.WriteFile(fakeHost, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pix-host: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "TestRouteAliasStillDispatches")
	cmd.Env = append(envWithout(os.Environ(), "PATH"),
		"PIX_ROUTE_ALIAS_TEST=1",
		"PATH="+dir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("route-alias subprocess failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	if !strings.Contains(stderr.String(), "pix route is now pix models") {
		t.Errorf("expected the deprecation notice on stderr, got:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "pix route is now pix models") {
		t.Errorf("deprecation notice must never reach stdout (breaks --json/piped output), got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ARGV:route show --json") {
		t.Errorf("`pix route show --json` must dispatch through to `pix-host route show --json` exactly like `pix models show --json` would, got stdout:\n%s", stdout.String())
	}
}

// TestRouteAliasForwardsEverySubcommandRaw is the regression test for the
// alias's whole reason to exist. `models` is a subcommand of the OLD verb
// (`pix route models` listed the registry) and is NOT a subcommand of the new
// one (`pix models ls` replaced it). Routing the alias through runModels
// therefore turned `pix route models --json` into "unknown subcommand" +
// usage on STDOUT + exit 2 — silently breaking the one-release compatibility
// the deprecation notice promises, for a caller who is piping to jq.
//
// Forwarding raw makes the alias bug-for-bug the command it replaces, so this
// walks the old spellings that have no new-verb equivalent.
func TestRouteAliasForwardsEverySubcommandRaw(t *testing.T) {
	if sub := os.Getenv("PIX_ROUTE_RAW_TEST"); sub != "" {
		os.Args = append([]string{"pix", "route"}, strings.Fields(sub)...)
		main()
		return
	}

	dir := t.TempDir()
	fakeHost := filepath.Join(dir, "pix-host")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo dev; exit 0; fi\necho \"ARGV:$@\"\n"
	if err := os.WriteFile(fakeHost, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pix-host: %v", err)
	}

	// `models` is the case that regressed; the others guard the same shape.
	for _, sub := range []string{"models", "models --json", "compile", "pick code"} {
		sub := sub
		t.Run(sub, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "TestRouteAliasForwardsEverySubcommandRaw")
			cmd.Env = append(envWithout(os.Environ(), "PATH"),
				"PIX_ROUTE_RAW_TEST="+sub,
				"PATH="+dir,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("`pix route %s` must still exit 0: %v\nstdout:\n%s\nstderr:\n%s", sub, err, stdout.String(), stderr.String())
			}
			if want := "ARGV:route " + sub; !strings.Contains(stdout.String(), want) {
				t.Errorf("`pix route %s` must forward raw to %q, got stdout:\n%s", sub, want, stdout.String())
			}
			// The payload is the ONLY thing on stdout: no usage prose, no notice.
			if strings.Contains(stdout.String(), "usage: pix models") {
				t.Errorf("`pix route %s` put usage on stdout, which breaks a piped --json caller:\n%s", sub, stdout.String())
			}
		})
	}
}

// TestNoRawPixRouteInProductionSource is a SOURCE guard, modeled on
// TestNoRawGogAuthLoginInProductionSource (copy_guard_test.go): it scans every
// production .go file in this package for the retired `pix route` phrase (in
// a string literal OR a comment) and fails the build if it ever reappears as
// GUIDANCE (something telling a user to type the old spelling).
// `pix-host route` is a DIFFERENT string (no space after "pix") so it needs no
// allowlist — the host subcommand tree is unaffected by this rename
// (docs/design/models-cli.md: "pix-host route does not move").
//
// ONE narrow, explicit exception: the deprecation notice itself
// ("pix route is now pix models ...", main.go's `case "route"`) legitimately
// names the retired spelling to ANNOUNCE its retirement — the opposite of
// guidance to use it. Matching that exact phrase is a deliberate allowlist,
// not a loophole: it is anchored to the one line that must say "is now",
// so any OTHER `pix route ...` string still fails the guard, including a
// regression of this same notice with different wording.
func TestNoRawPixRouteInProductionSource(t *testing.T) {
	re := regexp.MustCompile(`\bpix route\b`)
	const deprecationNotice = "pix route is now pix models"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var hits []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !re.MatchString(line) {
				continue
			}
			// The guard is about what a USER can be told, so it inspects string
			// literals only. A `//` comment naming the retired verb is how the
			// rename documents itself ("routing the alias through runModels broke
			// `pix route models`") and banning that would force the next reader to
			// rediscover the bug. A comment cannot reach stdout; a literal can.
			if !strings.Contains(line, `"`) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, deprecationNotice) {
				continue // the retirement announcement, not guidance — see doc comment
			}
			hits = append(hits, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	if len(hits) > 0 {
		t.Errorf("raw retired `pix route` guidance is banned from production source (use `pix models` / modelsUsage instead), found:\n%s",
			strings.Join(hits, "\n"))
	}
}
