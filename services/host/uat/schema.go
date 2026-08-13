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
		"mcp_add":        true,
		"mcp_auth":       true,
		"mcp_remove":     true,
		"mcp_call":       true,
		"browser_link":   true,
		"browser_action": true,
		"dry_run":        true,
		"submit":         true,
		"status":         true,
		"abort":          true,
		"read_artifact":  true,
		"status_probe":   true,
	}
	if !allowedActions[step.Do] {
		return fmt.Errorf("steps[%d]: unknown action: %s", index, step.Do)
	}

	forbidden := []string{"shell", "command", "argv", "env", "url"}
	return validateForbiddenKeys(&step.With, forbidden, fmt.Sprintf("steps[%d].with", index))
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

	forbidden := []string{"shell", "command", "argv", "env", "url"}
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
