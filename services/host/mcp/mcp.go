package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// ErrSbxUnavailable identifies a missing sandbox CLI.
var ErrSbxUnavailable = fmt.Errorf("sbx not on PATH")

// runSbxCaptured keeps registration evidence separate from diagnostics.
func runSbxCaptured(args []string) (stdout, stderr string, err error) {
	cmd := exec.Command("sbx", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// VerifyExistingEndpoint compares a registration's canonical endpoint. An
// unreadable registration is unverified, never evidence that it can be replaced.
func VerifyExistingEndpoint(name, want string) (matches bool, verified bool) {
	for _, verb := range []string{"inspect", "get"} {
		stdout, _, err := runSbxCaptured([]string{"mcp", verb, name})
		if err == nil {
			return outputContainsCanonicalEndpoint(stdout, want), true
		}
	}
	return false, false
}

// scopedOpRun reads references at process start, so rotations do not require
// rewriting Gateway registrations. Only declared keys reach op. Process
// substitution keeps the filtered references off disk; exec preserves signals.
const scopedOpRun = `op=$1; refs=$2; keys=$3; shift 3
selected=$(awk -v names="$keys" '
  BEGIN { split(names, list, ","); for (i in list) wanted[list[i]]=1 }
  { key=$0; sub(/^[[:space:]]*(export[[:space:]]+)?/, "", key)
    sub(/[[:space:]]*=.*/, "", key); if (key in wanted) print }
' "$refs") || exit
exec "$op" run --no-masking --env-file=<(printf '%s\n' "$selected") -- "$@"`

// OpRunWrap supplies only one integration's declared secret and plain keys.
// The argv contains paths and key names, never resolved credential values.
func OpRunWrap(opPath, opRefs string, keys, argv []string) []string {
	if opPath == "" || opRefs == "" || len(keys) == 0 || len(argv) == 0 {
		return argv
	}
	return append([]string{"/bin/bash", "-c", scopedOpRun, "pix-mcp", opPath, opRefs, strings.Join(keys, ",")}, argv...)
}

// outputContainsCanonicalEndpoint accepts only URL/endpoint fields (or a bare
// URL line) whose parsed canonical URL equals want. Arbitrary JSON strings and
// substrings are not identity evidence.
func outputContainsCanonicalEndpoint(out, want string) bool {
	wantURL, ok := canonicalMCPEndpoint(want)
	if !ok {
		return false
	}
	var decoded any
	if json.Unmarshal([]byte(out), &decoded) == nil {
		found := map[string]bool{}
		jsonCollectCanonicalEndpoints(decoded, found)
		return len(found) == 1 && found[wantURL]
	}
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if got, valid := canonicalMCPEndpoint(strings.Trim(line, `"'`)); valid {
			found[got] = true
		}
		if i := strings.Index(line, ":"); i >= 0 {
			key := normalizeEndpointField(line[:i])
			v := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
			if endpointField(key) {
				if got, valid := canonicalMCPEndpoint(v); valid {
					found[got] = true
				}
			}
		}
	}
	return len(found) == 1 && found[wantURL]
}

func jsonCollectCanonicalEndpoints(v any, found map[string]bool) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			jsonCollectCanonicalEndpoints(item, found)
		}
	case map[string]any:
		for key, item := range x {
			if endpointField(normalizeEndpointField(key)) {
				if raw, ok := item.(string); ok {
					if got, valid := canonicalMCPEndpoint(raw); valid {
						found[got] = true
					}
				}
			}
			jsonCollectCanonicalEndpoints(item, found)
		}
	}
}

func normalizeEndpointField(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
}

func endpointField(key string) bool {
	switch key {
	case "url", "endpoint", "remoteurl", "serverurl":
		return true
	default:
		return false
	}
}

func canonicalMCPEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.User != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Fragment != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	if port != "" {
		u.Host += ":" + port
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = u.Query().Encode()
	u.RawFragment = ""
	return u.String(), true
}

// AllPreloadedMCP returns, order-preserving and de-duplicated, every non-empty
// name in `servers` — the full set to attach EAGERLY at create (emitted to sbx
// as --static-mcp; their tools sit in context from the start). There is no
// eager/lazy split: every configured server, and every pack integration's
// server, preloads at CREATE regardless of kind, so this is pure list hygiene.
func AllPreloadedMCP(servers []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range servers {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
