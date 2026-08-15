package uat

import "testing"

func TestNameForSandbox(t *testing.T) {
	name, err := NameForSandbox("123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "pix-uat-123" {
		t.Errorf("expected pix-uat-123, got %s", name)
	}
}

func TestNameForMCP(t *testing.T) {
	name, err := NameForMCP("123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "pix-uat-123" {
		t.Errorf("expected pix-uat-123, got %s", name)
	}
}
