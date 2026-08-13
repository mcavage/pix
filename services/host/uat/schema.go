package uat

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Scenario struct {
	Schema  string   `yaml:"schema"`
	Name    string   `yaml:"name"`
	Timeout string   `yaml:"timeout"`
	Needs   []string `yaml:"needs"`
	Steps   []Step   `yaml:"steps"`
}

type Step struct {
	ID     string    `yaml:"id"`
	Do     string    `yaml:"do"`
	With   yaml.Node `yaml:"with"`
	Expect yaml.Node `yaml:"expect"`
}

func UnmarshalScenario(data []byte) (*Scenario, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	var s Scenario
	if err := decoder.Decode(&s); err != nil {
		return nil, err
	}

	// Check for single document
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("multiple documents not allowed")
	}

	if s.Schema != "pix.uat/1" {
		return nil, errors.New("invalid schema")
	}

	if s.Name == "" {
		return nil, errors.New("name is required")
	}

	if s.Timeout == "" {
		return nil, errors.New("timeout must be specified")
	}
	timeoutDur, err := time.ParseDuration(s.Timeout)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout format: %w", err)
	}
	if timeoutDur <= 0 {
		return nil, errors.New("timeout must be positive")
	}

	if len(s.Steps) == 0 {
		return nil, errors.New("at least one step is required")
	}

	allowedNeeds := map[string]bool{
		"mcp":     true,
		"browser": true,
		"docker":  true,
		"sbx":     true,
	}
	for _, n := range s.Needs {
		if !allowedNeeds[n] {
			return nil, fmt.Errorf("invalid need: %s", n)
		}
	}

	seenIDs := make(map[string]bool)
	for i, step := range s.Steps {
		if step.ID == "" {
			return nil, fmt.Errorf("steps[%d]: step ID is required", i)
		}
		if seenIDs[step.ID] {
			return nil, fmt.Errorf("steps[%d]: duplicate step ID: %s", i, step.ID)
		}
		seenIDs[step.ID] = true

		if step.Do == "" {
			return nil, fmt.Errorf("steps[%d]: do action is required", i)
		}

		if err := validateStepWith(&step, i); err != nil {
			return nil, err
		}
		if err := validateStepExpect(&step, i); err != nil {
			return nil, err
		}
	}

	return &s, nil
}

func validateStepWith(step *Step, index int) error {
	allowedActions := map[string]bool{
		"mcp_add":         true,
		"mcp_auth":        true,
		"mcp_status":      true,
		"mcp_remove":      true,
		"candidate_smoke": true,
		"check":           true,
		"browser_check":   true,
	}
	if !allowedActions[step.Do] {
		return fmt.Errorf("steps[%d]: unknown action: %s", index, step.Do)
	}

	forbidden := []string{"shell", "command", "argv", "env"}
	if step.Do != "browser_check" {
		forbidden = append(forbidden, "url")
	}
	if err := validateForbiddenKeys(&step.With, forbidden, fmt.Sprintf("steps[%d].with", index)); err != nil {
		return err
	}

	// Validate action-specific fields
	switch step.Do {
	case "mcp_add", "mcp_auth", "mcp_status", "mcp_remove":
		if !hasKey(&step.With, "name") {
			return fmt.Errorf("steps[%d]: action '%s' requires 'name' in 'with'", index, step.Do)
		}
	case "candidate_smoke":
		if step.With.Kind == yaml.MappingNode && len(step.With.Content) > 0 {
			return fmt.Errorf("steps[%d]: action 'candidate_smoke' does not accept 'with' keys", index)
		}
	case "browser_check":
		if !hasKey(&step.With, "url") {
			return fmt.Errorf("steps[%d]: action 'browser_check' requires 'url' in 'with'", index)
		}
	}
	return nil
}

func hasKey(node *yaml.Node, targetKey string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == targetKey {
			return true
		}
	}
	return false
}

func validateStepExpect(step *Step, index int) error {
	if step.Expect.Kind == yaml.MappingNode {
		allowedAssertions := map[string]bool{
			"mcp_status":        true,
			"browser_url":       true,
			"browser_text":      true,
			"artifact_exists":   true,
			"artifact_contains": true,
			"verdict":           true,
		}
		for i := 0; i < len(step.Expect.Content); i += 2 {
			keyNode := step.Expect.Content[i]
			if !allowedAssertions[keyNode.Value] {
				return fmt.Errorf("steps[%d].expect: unknown assertion: %s", index, keyNode.Value)
			}
		}
	}

	forbidden := []string{"shell", "command", "argv", "env"}
	if step.Do != "browser_check" {
		forbidden = append(forbidden, "url")
	}
	return validateForbiddenKeys(&step.Expect, forbidden, fmt.Sprintf("steps[%d].expect", index))
}

func validateForbiddenKeys(node *yaml.Node, forbidden []string, path string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		for _, forbiddenKey := range forbidden {
			if keyNode.Value == forbiddenKey {
				return fmt.Errorf("%s: forbidden key '%s' at line %d", path, forbiddenKey, keyNode.Line)
			}
		}
		if err := validateForbiddenKeys(valNode, forbidden, fmt.Sprintf("%s.%s", path, keyNode.Value)); err != nil {
			return err
		}
	}
	return nil
}
