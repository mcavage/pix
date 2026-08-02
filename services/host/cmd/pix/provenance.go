package main

// provenance.go reports where the running launcher was installed. Consumers
// use one detector so upgrade, doctor, status, and uninstall agree.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type installChannel int

const (
	channelUnknown installChannel = iota
	channelHomebrew
	channelInstaller
	channelLocalDev
)

func (c installChannel) String() string {
	switch c {
	case channelHomebrew:
		return "Homebrew"
	case channelInstaller:
		return "Installer"
	case channelLocalDev:
		return "LocalDev"
	default:
		return "?"
	}
}

type provenance struct {
	Channel  installChannel
	Resolved string
	Evidence string
}

func detectInstallChannel(
	executable func() (string, error),
	evalSymlinks func(string) (string, error),
	getenv func(string) string,
) provenance {
	self, err := executable()
	if err != nil {
		return provenance{Channel: channelUnknown, Evidence: "os.Executable failed: " + err.Error()}
	}
	resolved, err := evalSymlinks(self)
	if err != nil {
		return provenance{Channel: channelUnknown, Resolved: self, Evidence: "EvalSymlinks failed: " + err.Error()}
	}

	if !isReleased(version) {
		return provenance{Channel: channelLocalDev, Resolved: resolved, Evidence: "unreleased build (" + version + ")"}
	}

	if hasPathSequence(resolved, "Cellar", "pix") {
		evidence := "resolved path contains /Cellar/pix/"
		if prefix := getenv("HOMEBREW_PREFIX"); prefix != "" {
			prefix = filepath.Clean(prefix)
			if pathWithin(resolved, prefix) {
				evidence += ", under $HOMEBREW_PREFIX"
			} else {
				evidence += ", but outside $HOMEBREW_PREFIX (" + prefix + "); unusual, still treating as Homebrew"
			}
		}
		return provenance{Channel: channelHomebrew, Resolved: resolved, Evidence: evidence}
	}

	return provenance{Channel: channelInstaller, Resolved: resolved, Evidence: "resolved path: " + resolved}
}

func hasPathSequence(path string, sequence ...string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := 0; i+len(sequence) <= len(parts); i++ {
		matched := true
		for j, want := range sequence {
			if parts[i+j] != want {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func installChannelNow() provenance {
	return detectInstallChannel(os.Executable, filepath.EvalSymlinks, os.Getenv)
}

// allOnPath returns every distinct path spelling for name in PATH order. PATH
// itself may repeat directories, so exact duplicates are suppressed.
func allOnPath(name string, getenv func(string) string) []string {
	var found []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || seen[path] {
			continue
		}
		seen[path] = true
		found = append(found, path)
	}
	return found
}

// pathShadowIssue reports multiple distinct installations and true shadows.
// It is read-only: the suggested fix is explicit and never executed here.
func pathShadowIssue(name, self string, getenv func(string) string) string {
	paths := allOnPath(name, getenv)
	if len(paths) == 0 {
		return ""
	}

	resolved := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			real = path
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		resolved = append(resolved, path)
	}
	if len(resolved) < 2 {
		return ""
	}

	selfReal, err := filepath.EvalSymlinks(self)
	if err != nil {
		selfReal = self
	}
	firstReal, err := filepath.EvalSymlinks(resolved[0])
	if err != nil {
		firstReal = resolved[0]
	}

	var message string
	if firstReal != selfReal {
		message = fmt.Sprintf("%s on PATH resolves to %s, not the running binary (%s).", name, resolved[0], self)
	} else {
		message = fmt.Sprintf("multiple %s installations are on PATH; the active one is %s.", name, resolved[0])
	}
	message += "\n  also found: " + strings.Join(resolved[1:], ", ")
	message += "\n  fix: put the intended install first on PATH, or remove the stale copy explicitly."
	return message
}

func installDuplicatesGroup(env shellEnv) group {
	g := group{title: "Installation"}

	self, err := env.Executable()
	if err == nil {
		if warning := pathShadowIssue("pix", self, env.Getenv); warning != "" {
			g.checks = append(g.checks, check{label: "pix PATH", detail: warning, evidence: warning, verdict: verdictTodo})
		}
	}
	if env.HostBinary != nil {
		if host, err := env.HostBinary(); err == nil {
			if warning := pathShadowIssue("pix-host", host, env.Getenv); warning != "" {
				g.checks = append(g.checks, check{label: "host PATH", detail: warning, evidence: warning, verdict: verdictTodo})
			}
		}
	}
	return g
}
