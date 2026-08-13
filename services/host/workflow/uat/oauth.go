package uat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type OAuthCapture struct {
	RunID    string `json:"run_id"`
	Callback string `json:"callback,omitempty"`
	Origin   string `json:"origin"`
	Error    string `json:"error,omitempty"`
}

type OAuthConfig struct {
	RunID       string
	AuthURL     string
	CallbackURL string
	Origin      string
	Policy      URLValidator
}

func CaptureOAuth(ctx context.Context, factory BrowserFactory, cfg OAuthConfig, outPath string) error {
	if cfg.RunID == "" {
		return fmt.Errorf("missing run ID")
	}

	authU, err := cfg.Policy.Validate(cfg.AuthURL)
	if err != nil {
		return fmt.Errorf("invalid auth URL: %w", err)
	}

	callbackU, err := cfg.Policy.Validate(cfg.CallbackURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}

	if authU.Hostname() != cfg.Origin {
		return fmt.Errorf("auth URL origin %q does not match expected %q", authU.Hostname(), cfg.Origin)
	}

	b, err := factory.NewOAuthContext(ctx, authU, cfg.Policy)
	if err != nil {
		return fmt.Errorf("new oauth context: %w", err)
	}
	defer b.Close()

	// Wait for the callback URL
	err = b.WaitForURL(ctx, callbackU)

	cap := OAuthCapture{
		RunID:  cfg.RunID,
		Origin: cfg.Origin,
	}

	if err != nil {
		if errors.Is(err, ErrIncomplete) {
			cap.Error = "incomplete"
		} else {
			cap.Error = err.Error()
		}
	} else {
		finalU, cerr := b.CurrentURL(ctx)
		if cerr != nil {
			cap.Error = cerr.Error()
		} else {
			cap.Callback = finalU.String()
		}
	}

	data, err := json.Marshal(cap)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create capture file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}
