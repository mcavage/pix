package envinfo

import "regexp"

// interpolationRE matches one `${VAR}` or `${VAR:-default}` host-variable
// reference. It does not attempt to resolve either capture group against
// anything — see doc.go's "Interpolation is surfaced, never resolved".
var interpolationRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// scanInterpolations returns one Interpolation per `${VAR}`/`${VAR:-def}`
// reference found in value, attributed to keyPath. It never inspects the
// host environment: it is a pure string scan.
func scanInterpolations(value, keyPath string) []Interpolation {
	if value == "" {
		return nil
	}
	matches := interpolationRE.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Interpolation, 0, len(matches))
	for _, m := range matches {
		item := Interpolation{Var: m[1], KeyPath: keyPath}
		if m[2] != "" {
			def := m[3]
			item.Default = &def
		}
		out = append(out, item)
	}
	return out
}
