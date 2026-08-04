package main

// models_rename_test.go covers the `route` -> `models` verb rename
// (docs/design/models-cli.md). The one-release deprecation alias is now
// RETIRED (retired.go, corpus/retirement.jsonl), so what is left here is the
// source guard: the old spelling never regresses into production guidance.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

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
