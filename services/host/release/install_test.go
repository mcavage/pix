package release_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/release"
)

// bundleFor writes an installation tree whose archive carries `extra`
// entries on top of the canonical layout, and returns the loaded bundle.
func bundleFor(t *testing.T, version string, extra func(tw *tar.Writer)) release.Bundle {
	t.Helper()
	dir := t.TempDir()
	archive, digest := runtimeArchive(t, version, extra)
	if err := os.WriteFile(filepath.Join(dir, release.BundleManifestFile), shippedManifestJSON(version, digestA, digestA, digest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.RuntimeArchiveName(version)), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := release.LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	return *b
}

func TestInstallRuntimeExtractsTheCanonicalLayout(t *testing.T) {
	home := t.TempDir()
	b := bundleFor(t, "1.2.3", nil)

	res, err := release.InstallRuntime(home, b)
	if err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	if !res.Extracted {
		t.Fatal("first install must extract")
	}
	for _, rel := range []string{"skills/plan/SKILL.md", "agents/deep.md", "pi/settings.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(release.RuntimeDir(home, "1.2.3"), rel)); err != nil {
			t.Fatalf("runtime is missing %s: %v", rel, err)
		}
	}

	// Idempotent: a rerun neither re-extracts nor errors.
	again, err := release.InstallRuntime(home, b)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again.Extracted {
		t.Fatal("a rerun against the same digest must not re-extract")
	}
}

func TestInstallRuntimeNeverTouchesUserOwnedGlobals(t *testing.T) {
	home := t.TempDir()
	// The user's own forks, exactly the paths surface §4.2 says a release
	// install must never overwrite.
	globals := map[string]string{
		"skills/mine/SKILL.md": "user skill\n",
		"agents/deep.md":       "user fork\n",
		"pi/settings.json":     "{\"user\":true}\n",
		"pi/keybindings.json":  "{\"user\":true}\n",
		"pi/themes/mine.json":  "{\"user\":true}\n",
	}
	for rel, content := range globals {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := release.InstallRuntime(home, bundleFor(t, "1.2.3", nil)); err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}

	for rel, content := range globals {
		got, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("user file %s disappeared: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("user file %s was overwritten by the release install: %q", rel, got)
		}
	}
}

// Every hostile archive shape, each one refused OUTRIGHT (not skipped), with
// nothing published: a failed install must leave no runtime/<version> for a
// later launch to read.
func TestInstallRuntimeRefusesHostileArchives(t *testing.T) {
	cases := map[string]struct {
		entries func(tw *tar.Writer)
		want    string
	}{
		"absolute path": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "/etc/passwd", tar.TypeReg, "")
			},
			want: "absolute",
		},
		"parent traversal": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/1.2.3/../../../escaped.txt", tar.TypeReg, "")
			},
			want: "traversal",
		},
		"outside the canonical prefix": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "skills/evil/SKILL.md", tar.TypeReg, "")
			},
			want: "outside the canonical",
		},
		"wrong version prefix": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/9.9.9/skills/x.md", tar.TypeReg, "")
			},
			want: "outside the canonical",
		},
		"symlink": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/1.2.3/link", tar.TypeSymlink, "../../../../etc/passwd")
			},
			want: "only regular files and directories",
		},
		"hardlink": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/1.2.3/hard", tar.TypeLink, "runtime/1.2.3/manifest.json")
			},
			want: "only regular files and directories",
		},
		"character device": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/1.2.3/dev", tar.TypeChar, "")
			},
			want: "only regular files and directories",
		},
		"fifo": {
			entries: func(tw *tar.Writer) {
				writeHostileFile(t, tw, "runtime/1.2.3/pipe", tar.TypeFifo, "")
			},
			want: "only regular files and directories",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			sentinel := filepath.Join(home, "skills", "keep.md")
			if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinel, []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := release.InstallRuntime(home, bundleFor(t, "1.2.3", tc.entries))
			if err == nil {
				t.Fatal("a hostile archive must fail the install")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must explain the refusal (%q), got: %v", tc.want, err)
			}
			if _, err := os.Stat(release.RuntimeDir(home, "1.2.3")); !os.IsNotExist(err) {
				t.Fatalf("a failed install must publish nothing: %v", err)
			}
			if got, _ := os.ReadFile(sentinel); string(got) != "keep\n" {
				t.Fatalf("a failed install touched user state: %q", got)
			}
			if _, err := os.Stat(filepath.Join(home, "escaped.txt")); !os.IsNotExist(err) {
				t.Fatal("traversal escaped the runtime directory")
			}
			// Staging leaves nothing behind either.
			entries, err := os.ReadDir(filepath.Join(home, "runtime"))
			if err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".stage-") {
						t.Fatalf("staging directory %s survived a failed install", e.Name())
					}
				}
			}
		})
	}
}

func TestInstallRuntimeReplacesAStaleSameVersionInstall(t *testing.T) {
	home := t.TempDir()
	dir := release.RuntimeDir(home, "1.2.3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A partial/old install: no stamp, plus a file the new archive lacks.
	if err := os.WriteFile(filepath.Join(dir, "stale.md"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := release.InstallRuntime(home, bundleFor(t, "1.2.3", nil))
	if err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	if !res.Extracted {
		t.Fatal("an unstamped prior install must be replaced, not trusted")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.md")); !os.IsNotExist(err) {
		t.Fatal("replacement must not merge the stale tree into the new one")
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("new runtime content missing: %v", err)
	}
}

// TestInstallRuntimeCannotEscapeViaMaliciousVersion is the defense-in-depth
// proof: Version flows unquoted into RuntimeDir (filepath.Join(home,
// "runtime", version)) and into extractRuntimeArchive's tar-entry prefix.
// A manifest naming a traversal-shaped version must never reach
// InstallRuntime at all — LoadBundle's ParseBundleManifest already calls
// Manifest.Validate, so the malicious document is refused at load time,
// long before any archive is staged or extracted.
func TestInstallRuntimeCannotEscapeViaMaliciousVersion(t *testing.T) {
	for _, version := range []string{
		"../../escape",
		"..",
		"1.2.3/../../escape",
		"/etc/passwd",
	} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			archive, digest := runtimeArchive(t, "1.2.3", nil)
			if err := os.WriteFile(filepath.Join(dir, release.BundleManifestFile), shippedManifestJSON(version, digestA, digestA, digest), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, release.RuntimeArchiveName("1.2.3")), archive, 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := release.LoadBundle(dir)
			if err == nil {
				t.Fatalf("LoadBundle accepted a manifest naming version %q; a traversal-shaped version must be refused before any archive is staged", version)
			}
			if !strings.Contains(err.Error(), "version") {
				t.Errorf("LoadBundle error = %q, want it to name the version problem", err)
			}

			// Nothing above home (a sibling of the temp home, standing in for
			// "outside the intended runtime root") was ever created.
			home := t.TempDir()
			escaped := filepath.Join(filepath.Dir(home), "escape")
			if _, statErr := os.Stat(escaped); !os.IsNotExist(statErr) {
				t.Fatalf("a path escaping the runtime root was created: %s", escaped)
			}
		})
	}
}

func writeHostileFile(t *testing.T, tw *tar.Writer, name string, typeflag byte, link string) {
	t.Helper()
	body := "x\n"
	hdr := &tar.Header{Name: name, Typeflag: typeflag, Mode: 0o644, Linkname: link}
	if typeflag == tar.TypeReg {
		hdr.Size = int64(len(body))
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
}
