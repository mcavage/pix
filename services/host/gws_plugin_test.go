package main

import (
	"testing"

	"pi-stack/host/plugin"
)

// Compile-time proof the adapter satisfies the broker interface.
var _ plugin.CredentialBroker = (*gwsBrokerAdapter)(nil)

func TestGwsBrokerDescribe(t *testing.T) {
	a := newGwsBrokerAdapter()
	got, err := a.Describe()
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	want := plugin.BrokerInfo{
		Name:            "gws",
		DefaultPort:     11441,
		AuthHeader:      "Authorization",
		RequiresHostCLI: true,
	}
	if got != want {
		t.Errorf("Describe() = %+v, want %+v", got, want)
	}
}

// TestGwsBrokerCheck exercises the Check path. It must not panic regardless of
// whether the host `gws` is present/authenticated. A missing or unauthenticated
// CLI yields a clean, non-empty error; a fully-authenticated host yields nil.
func TestGwsBrokerCheck(t *testing.T) {
	a := newGwsBrokerAdapter()
	if err := a.Check(); err != nil && err.Error() == "" {
		t.Fatal("Check() returned an error with an empty message")
	}
}

// TestGwsBrokerMint exercises the Mint path. As with Check, the only invariant
// that holds in every environment is: no panic, and on error a clean message.
// On success (a live authenticated host) it must NOT leak the raw long-lived
// credential — Mint returns only a short-lived Token, never client_secret /
// refresh_token (those types are unreachable through the interface).
func TestGwsBrokerMint(t *testing.T) {
	a := newGwsBrokerAdapter()
	tok, err := a.Mint("", nil)
	if err != nil {
		if err.Error() == "" {
			t.Fatal("Mint() returned an error with an empty message")
		}
		return // expected in a sandbox without an authenticated host gws
	}
	if tok.AccessToken == "" {
		t.Error("Mint() succeeded but returned an empty AccessToken")
	}
}
