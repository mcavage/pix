package uat

import "fmt"

func NameForSandbox(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("pix-uat-%s", id), nil
}

func NameForMCP(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("pix-uat-%s", id), nil
}
