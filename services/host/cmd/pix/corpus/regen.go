package corpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrWholesaleRegeneration is returned by ValidateRegenTarget for any target
// that would rewrite more than one shard at once.
var ErrWholesaleRegeneration = errors.New("wholesale baseline regeneration is forbidden: pass exactly one verb")

// wholesaleTargets are the spellings a careless "regenerate everything" call
// might use; all of them are refused, not just the empty string.
var wholesaleTargets = map[string]bool{"": true, "all": true, "*": true, "-": true}

// ValidateRegenTarget enforces that a corpus-baseline regeneration names
// exactly one verb. The harness has no "regenerate all shards" mode at all —
// there is no argument that means that — so a baseline update is always a
// small, individually reviewable diff to one file, never a silent mass
// rewrite that could launder a real regression as "the new expected output".
func ValidateRegenTarget(target string) error {
	norm := strings.ToLower(strings.TrimSpace(target))
	if wholesaleTargets[norm] {
		return fmt.Errorf("%w (got %q)", ErrWholesaleRegeneration, target)
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w (empty target)", ErrWholesaleRegeneration)
	}
	return nil
}

// RegenerateShard writes exactly one shard file (dir/<verb>.json), after
// validating it. It never touches any other file in dir, and it refuses to
// write an empty shard — a golden baseline that proves nothing is worse than
// no baseline at all, because it would pass forever.
func RegenerateShard(dir string, s Shard) error {
	if err := ValidateRegenTarget(s.Verb); err != nil {
		return err
	}
	if err := ValidateShard(s); err != nil {
		return fmt.Errorf("corpus: refusing to write an invalid shard: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("corpus: marshal shard %q: %w", s.Verb, err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, s.Verb+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("corpus: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("corpus: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
