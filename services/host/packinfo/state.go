// state.go — the resolved read-only state of the pack that is active on this
// host: which root is active, whether it loads, and which conventional facets
// it carries. It is ONE derivation because doctor, launch's trusted host state
// and setup's pack probe must never disagree about what "active" means.
package packinfo

import (
	"os"
	"path/filepath"

	"pix/host/config"
)

type State struct {
	Active         bool   `json:"active"`          // a pack is ACTUALLY active (config `pack` or a --pack override)
	Exists         bool   `json:"exists"`          // a loadable pack exists at Path
	Default        bool   `json:"default"`         // Path IS the default pack root
	Path           string `json:"path"`            // the active pack's root, or the default's when none is active
	GitInitialized bool   `json:"git_initialized"` // has a .git
	Skills         bool   `json:"skills"`          // has skills/
	Knowledge      bool   `json:"knowledge"`       // has knowledge/
}

func Resolve(cfg *config.Config, override string) State {
	root := ActivePackRoot(cfg.Pack, override)
	active := root != ""
	if root == "" {
		root = DefaultPackRoot() // runs the legacy pack/personal -> default migration
	}
	p, err := LoadPack(root)
	if err != nil {
		return State{}
	}
	_, gitErr := os.Stat(filepath.Join(root, ".git"))
	return State{
		Active:         active,
		Exists:         true,
		Default:        CanonicalizePackRoot(p.Root) == CanonicalizePackRoot(DefaultPackRoot()),
		Path:           p.Root,
		GitInitialized: gitErr == nil,
		Skills:         p.SkillsDir != "",
		Knowledge:      p.KnowledgeDir != "",
	}
}

// LaunchContribution is what an active, TRUST-VERIFIED pack stack adds to ONE
// sandbox launch: extra skill directories to mount, ephemeral mixin kits
// carrying the packs' sandbox bin/ wrappers, and MCP server names to preload at
// create. Root is the LAST effectively-applied pack root, or "" when no pack
// applied.
//
// It is a VALUE, and it lives here rather than in either workflow, because the
// two halves of the job are owned by different layers: workflow/pack verifies
// the trust surface and produces this; workflow/launch folds it into a launch.
// Neither may import the other, so the thing they exchange has a lower home —
// the same inversion `mcp.Credentials` and `config.MCPServer` already use.
type LaunchContribution struct {
	Root     string
	Skills   []string
	Kits     []string
	MCPNames []string
}
