package uat

import (
	"testing"
)

func TestValidateID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid", "run-123", false},
		{"empty", "", true},
		{"too long", "a-very-long-id-that-is-definitely-more-than-sixty-four-characters-long-and-should-fail", true},
		{"invalid chars", "run_123", true},
		{"leading hyphen", "-run", true},
		{"trailing hyphen", "run-", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateID(tt.id); (err != nil) != tt.wantErr {
				t.Errorf("ValidateID() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
