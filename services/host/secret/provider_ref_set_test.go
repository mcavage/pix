package secret

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

func TestProviderRefSet(t *testing.T) {
	mk := func(refs string) hostenv.Env {
		return hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return "/cfg"
			}
			return ""
		}, ReadFileFn: func(p string) (string, error) {
			if p == filepath.Join("/cfg", "pix", "op-refs.env") {
				return refs, nil
			}
			return "", os.ErrNotExist
		}}}
	}
	if providerRefSet(mk(""), "ANTHROPIC_API_KEY") {
		t.Error("empty op-refs.env must report no ref")
	}
	if !providerRefSet(mk("ANTHROPIC_API_KEY=op://v/a/k\n"), "ANTHROPIC_API_KEY") {
		t.Error("a filled ref must be detected")
	}
	if providerRefSet(mk("OPENAI_API_KEY=op://v/o/k\n"), "ANTHROPIC_API_KEY") {
		t.Error("a different provider's ref must not count")
	}
}
