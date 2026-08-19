package uat

import (
	"errors"
	"fmt"
	"io"
	"slices"
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

// LegalVocabulary returns the exact closed scenario vocabulary accepted by
// UnmarshalScenario. Capability reporting uses the same slices as validation,
// so the advertised contract cannot drift from execution.
func LegalVocabulary() (needs, actions, assertions []string) {
	return []string{"browser", "docker", "mcp", "sbx"},
		[]string{"browser_check", "candidate_smoke", "mcp_add", "mcp_auth", "mcp_remove", "mcp_status"},
		[]string{"artifact_contains", "artifact_exists", "browser_text", "browser_url", "mcp_status", "verdict"}
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

	allowedNeeds, allowedActions, allowedAssertions := LegalVocabulary()
	for _, n := range s.Needs {
		if !slices.Contains(allowedNeeds, n) {
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

		if err := validateStepWith(&step, i, allowedActions); err != nil {
			return nil, err
		}
		if err := validateStepExpect(&step, i, allowedAssertions); err != nil {
			return nil, err
		}
	}

	return &s, nil
}

func validateStepWith(step *Step, index int, allowedActions []string) error {
	if !slices.Contains(allowedActions, step.Do) {
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

func validateStepExpect(step *Step, index int, allowedAssertions []string) error {
	if step.Expect.Kind == yaml.MappingNode {
		for i := 0; i < len(step.Expect.Content); i += 2 {
			keyNode := step.Expect.Content[i]
			if !slices.Contains(allowedAssertions, keyNode.Value) {
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
