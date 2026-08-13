package uat

import (
	"testing"
)

func TestGetToolDescriptors(t *testing.T) {
	descriptors := GetToolDescriptors()
	if len(descriptors) != 5 {
		t.Errorf("expected 5 descriptors, got %d", len(descriptors))
	}

	foundSubmit := false
	for _, d := range descriptors {
		if d.Name == "submit" {
			foundSubmit = true
		}
	}
	if !foundSubmit {
		t.Errorf("submit descriptor not found")
	}
}
