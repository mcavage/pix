package uat

import "testing"

func TestUnmarshalScenario_Strict(t *testing.T) {
	data := []byte(`
schema: pix.uat/1
name: test
unknown: field
steps:
  - id: step1
    do: action
  - id: step1
    do: action
`)
	_, err := UnmarshalScenario(data)
	if err == nil {
		t.Error("expected error due to unknown field and duplicate ID")
	}
}
