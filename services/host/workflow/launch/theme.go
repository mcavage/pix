package launch

import (
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"pix/host/config"
)

const (
	themePreferenceMaxBytes = 66
	themeNamePattern        = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
)

// ReadThemePreference reads the cosmetic, user-owned theme choice. Theme state
// must never block a launch: malformed, oversized, unreadable, or symlinked
// preferences fall back to Pi's shipped default.
func ReadThemePreference() string {
	path := filepath.Join(config.ContextDir(), "themes", "active")
	file, err := openNoFollow(path, syscall.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > themePreferenceMaxBytes {
		return ""
	}
	contents, err := io.ReadAll(io.LimitReader(file, themePreferenceMaxBytes+1))
	if err != nil || len(contents) > themePreferenceMaxBytes {
		return ""
	}
	value := strings.TrimSpace(string(contents))
	if matched, _ := regexp.MatchString(themeNamePattern, value); !matched {
		return ""
	}
	return value
}
