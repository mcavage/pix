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
	Schema  string        `yaml:"schema"`
	Name    string        `yaml:"name"`
	Timeout time.Duration `yaml:"timeout"`
	Needs   []string      `yaml:"needs"`
	Steps   []Step        `yaml:"steps"`
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

	if len(s.Steps) == 0 {
		return nil, errors.New("at least one step is required")
	}

	if s.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
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
	forbidden := []string{"shell", "command", "argv", "env", "url"}
	return validateForbiddenKeys(&step.With, forbidden, fmt.Sprintf("steps[%d].with", index))
}

func validateStepExpect(step *Step, index int) error {
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
