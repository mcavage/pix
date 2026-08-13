package uat

import (
	"errors"
	"fmt"
	"strings"

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
	ID     string                 `yaml:"id"`
	Do     string                 `yaml:"do"`
	With   map[string]interface{} `yaml:"with"`
	Expect map[string]interface{} `yaml:"expect"`
}

func UnmarshalScenario(data []byte) (*Scenario, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	var s Scenario
	if err := decoder.Decode(&s); err != nil {
		return nil, err
	}

	if s.Schema != "pix.uat/1" {
		return nil, errors.New("invalid schema")
	}

	if s.Name == "" {
		return nil, errors.New("name is required")
	}

	seenIDs := make(map[string]bool)
	for _, step := range s.Steps {
		if step.ID == "" {
			return nil, errors.New("step ID is required")
		}
		if seenIDs[step.ID] {
			return nil, fmt.Errorf("duplicate step ID: %s", step.ID)
		}
		seenIDs[step.ID] = true

		if step.Do == "" {
			return nil, fmt.Errorf("step %s: do action is required", step.ID)
		}
	}

	return &s, nil
}
