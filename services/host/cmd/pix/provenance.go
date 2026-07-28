package main

// provenance.go reports where the running launcher was installed. Consumers
// use one detector so upgrade, doctor, status, and uninstall agree.

import (
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
