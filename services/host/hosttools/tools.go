// Package hosttools implements Pix's compiled-in host operations. Privilege is
// supplied by the launcher, never by tool arguments. The Gateway owns transport.
package hosttools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/pixhome"
)

type Config struct {
	Home      pixhome.Paths
	Workspace string
	Pix       string
	Dev       bool
	// Guard proves this server still belongs to a live sandbox instance.
	Guard func() error
	// Redact is applied to every result, after capture and before disclosure.
	Redact func(string) string
}

type tool struct {
	Name, Description string
	Fields            map[string]any
	Required          []string
	Dev               bool
}

var definitions = []tool{
	{Name: "pix_host_info", Description: "Show this session's host workspace mapping and host tool privileges."},
	{Name: "pix_env_list", Description: "List host Pix environments without modifying them."},
	{Name: "pix_env_inspect", Description: "Inspect a named host environment or preview its effective document.", Fields: map[string]any{"name": str(), "effective": map[string]any{"type": "boolean"}}, Required: []string{"name"}},
	{Name: "pix_env_add", Description: "Validate and adopt an environment directory authored inside this session's workspace. Does not grant trust or change the default.", Fields: map[string]any{"source": str(), "name": str()}, Required: []string{"source", "name"}},
	{Name: "pix_env_test", Description: "Run a bounded prompt through a named environment in a separate host test workspace. Normal Pix trust checks apply. Output includes Pi JSON response metadata.", Fields: map[string]any{"name": str(), "prompt": str(), "model": str(), "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600}}, Required: []string{"name", "prompt"}},
	{Name: "pix_job_status", Description: "Read bounded, redacted output and exit status of a job started by this session.", Fields: map[string]any{"job_id": str()}, Required: []string{"job_id"}},
	{Name: "pix_job_cancel", Description: "Cancel a job started by this session.", Fields: map[string]any{"job_id": str()}, Required: []string{"job_id"}},
	{Name: "pix_host_exec", Description: "DEV ONLY: execute a host command with argv, including pix, sbx, git, and build tools. This grants the host user's authority; use a separate PIX_HOME for UAT. Returns a job ID for polling/cancellation.", Fields: map[string]any{"argv": map[string]any{"type": "array", "items": str(), "minItems": 1}, "cwd": str(), "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}}, Required: []string{"argv"}, Dev: true},
}

func str() map[string]any { return map[string]any{"type": "string"} }
func (s *Service) Definitions() []any {
	out := []any{}
	for _, t := range definitions {
		if t.Dev && !s.Config.Dev {
			continue
		}
		fields := t.Fields
		if fields == nil {
			fields = map[string]any{}
		}
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": map[string]any{"type": "object", "properties": fields, "required": append([]string{}, t.Required...), "additionalProperties": false}})
	}
	return out
}

type arguments struct {
	Name      string   `json:"name"`
	Source    string   `json:"source"`
	Effective bool     `json:"effective"`
	Prompt    string   `json:"prompt"`
	Model     string   `json:"model"`
	JobID     string   `json:"job_id"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Timeout   int      `json:"timeout_seconds"`
}

func (s *Service) Call(name string, raw json.RawMessage) (string, error) {
	if s.Config.Guard == nil {
		return "", errors.New("host tools have no live session")
	}
	if err := s.Config.Guard(); err != nil {
		return "", err
	}
	var spec *tool
	for i := range definitions {
		if definitions[i].Name == name {
			spec = &definitions[i]
			break
		}
	}
	if spec == nil || spec.Dev && !s.Config.Dev {
		return "", errors.New("tool unavailable in this session")
	}
	// Validate fields per tool even when callers bypass tools/list's schema.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", errors.New("arguments must be an object")
	}
	for k := range fields {
		if _, ok := spec.Fields[k]; !ok {
			return "", fmt.Errorf("unexpected argument %q", k)
		}
	}
	for _, k := range spec.Required {
		if _, ok := fields[k]; !ok {
			return "", fmt.Errorf("missing argument %q", k)
		}
	}
	var a arguments
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return "", errors.New("invalid tool arguments")
	}
	if a.Name != "" && (strings.HasPrefix(a.Name, "-") || strings.ContainsAny(a.Name, "/\\\x00")) {
		return "", errors.New("invalid environment name")
	}
	if name == "pix_host_info" {
		return encode(map[string]any{"workspace": s.Config.Workspace, "pix_home": s.Config.Home.Home, "development": s.Config.Dev, "pix": s.Config.Pix}), nil
	}
	if name == "pix_job_status" || name == "pix_job_cancel" {
		return s.status(a.JobID, name == "pix_job_cancel")
	}
	cwd := s.Config.Workspace
	argv := []string{s.Config.Pix}
	timeout := 60 * time.Second
	switch name {
	case "pix_env_list":
		argv = append(argv, "env", "list", "--json")
	case "pix_env_inspect":
		if a.Name == "" {
			return "", errors.New("name is required")
		}
		argv = append(argv, "env", "show", a.Name)
		if a.Effective {
			argv = append(argv, "--effective")
		} else {
			argv = append(argv, "--json")
		}
	case "pix_env_add":
		if a.Name == "" {
			return "", errors.New("name is required")
		}
		source, err := containedDirectory(cwd, a.Source)
		if err != nil {
			return "", err
		}
		argv = append(argv, "env", "add", source, a.Name)
	case "pix_env_test":
		if strings.HasPrefix(a.Model, "-") || strings.ContainsRune(a.Model, 0) {
			return "", errors.New("invalid model name")
		}
		if a.Name == "" || strings.TrimSpace(a.Prompt) == "" {
			return "", errors.New("name and prompt are required")
		}
		timeout = 10 * time.Minute
		if a.Timeout < 0 || a.Timeout > 600 {
			return "", errors.New("timeout must be between 1 and 600 seconds")
		}
		base := filepath.Join(s.Config.Home.State, "trials")
		if err := os.MkdirAll(base, 0700); err != nil {
			return "", err
		}
		var err error
		cwd, err = os.MkdirTemp(base, "environment-")
		if err != nil {
			return "", err
		}
		argv = append(argv, "run", cwd, "--env", a.Name)
		if a.Model != "" {
			argv = append(argv, "--model", a.Model)
		}
		argv = append(argv, "--", "--mode", "json", "--print", a.Prompt)
	case "pix_host_exec":
		if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
			return "", errors.New("argv must name a command")
		}
		argv = a.Argv
		timeout = 10 * time.Minute
		if a.Timeout < 0 || a.Timeout > 3600 {
			return "", errors.New("timeout must be between 1 and 3600 seconds")
		}
		if a.Cwd != "" {
			if filepath.IsAbs(a.Cwd) {
				cwd = a.Cwd
			} else {
				cwd = filepath.Join(cwd, a.Cwd)
			}
		}
	}
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	return s.start(argv, cwd, timeout, name == "pix_env_test")
}

func containedDirectory(root, source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("source directory is required")
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("environment source: %w", err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("environment source must be inside this session's host workspace")
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", errors.New("environment source must be a directory")
	}
	return real, nil
}
func encode(v any) string { b, _ := json.Marshal(v); return string(b) }
