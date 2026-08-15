package uat

import (
	"errors"
	"fmt"
	"regexp"
)

var (
	idRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
)

const MaxIDLength = 50

func ValidateID(id string) error {
	if len(id) == 0 {
		return errors.New("ID cannot be empty")
	}
	if len(id) > MaxIDLength {
		return fmt.Errorf("ID too long, max length is %d", MaxIDLength)
	}
	// Special case for single-character IDs
	if len(id) == 1 {
		if !regexp.MustCompile(`^[a-z0-9]$`).MatchString(id) {
			return errors.New("ID contains invalid characters")
		}
		return nil
	}

	if !idRegex.MatchString(id) {
		return errors.New("ID contains invalid characters or has leading/trailing hyphens")
	}
	return nil
}
