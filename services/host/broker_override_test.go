package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
	"pi-stack/host/plugin"
)

// buildExampleBroker compiles examples/broker-example to a temp binary and
// returns its path + sha256. This is the artifact a private overlay would ship
// and pin in config.toml's [plugins.broker].
func buildExampleBroker(t *testing.T) (bin, sha string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "broker-example")
	out, err := exec.Command("go", "build", "-o", bin, "./examples/broker-example").CombinedOutput()
	if err != nil {
		t.Fatalf("go build broker-example failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return bin, hex.EncodeToString(sum[:])
}

// TestExternalBrokerOverrideEndToEnd proves the OVERRIDE mechanism: an external
// broker binary is sha-verified, launched by the supervisor as a real
// out-of-process go-plugin subprocess, dispensed over net/rpc, and its Mint /
// Check / Describe round-trip. Then the stable /token shim (brokerProxyMux) is
// shown to proxy to that same dispensed client — exactly what the sandbox hits.
func TestExternalBrokerOverrideEndToEnd(t *testing.T) {
	bin, sha := buildExampleBroker(t)

	// A config a user would write to plug in an overlay broker (the dormant seam).
	spec := config.PluginSpec{Impl: "example", Path: bin, SHA: sha}

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("example", "broker", spec, "", nil)
	if err != nil {
		t.Fatalf("launch external broker: %v", err)
	}

	b, ok := h.get().(plugin.CredentialBroker)
	if !ok || b == nil {
		t.Fatalf("dispensed impl is not a CredentialBroker: %T", h.get())
	}

	// Mint round-trips over real RPC and honours the audience.
	tok, err := b.Mint("warehouse", []string{"read"})
	if err != nil {
		t.Fatalf("Mint over RPC: %v", err)
	}
	if tok.AccessToken != "example-token-warehouse" {
		t.Errorf("AccessToken = %q, want example-token-warehouse", tok.AccessToken)
	}
	if tok.ExpiresIn != 300 {
		t.Errorf("ExpiresIn = %d, want 300", tok.ExpiresIn)
	}

	// Check succeeds (the example is always authenticated).
	if err := b.Check(); err != nil {
		t.Errorf("Check over RPC: %v", err)
	}

	// Describe reports the example broker's shape.
	info, err := b.Describe()
	if err != nil {
		t.Fatalf("Describe over RPC: %v", err)
	}
	if info.Name != "example" || info.DefaultPort != 0 || info.RequiresHostCLI {
		t.Errorf("Describe = %+v, want {Name:example DefaultPort:0 RequiresHostCLI:false}", info)
	}

	// The /token shim proxies to the dispensed external broker: a correct
	// bearer yields the fake minted token (Mint("", nil) -> "example-token-").
	srv := httptest.NewServer(brokerProxyMux(h, "shim-secret"))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/token", nil)
	req.Header.Set("Authorization", "Bearer shim-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("shim /token status = %d, want 200", res.StatusCode)
	}
	var bearer brokerToken
	if err := json.NewDecoder(res.Body).Decode(&bearer); err != nil {
		t.Fatal(err)
	}
	if bearer.AccessToken != "example-token-" || bearer.ExpiresIn != 300 {
		t.Errorf("shim minted %+v, want AccessToken=example-token- ExpiresIn=300", bearer)
	}
}

// TestExternalBrokerRefusesOnSHAMismatch proves the pinned-checksum gate: the
// same real example binary is refused at launch when config pins the wrong sha,
// so no subprocess is ever spawned.
func TestExternalBrokerRefusesOnSHAMismatch(t *testing.T) {
	bin, sha := buildExampleBroker(t)

	// Flip the last hex nibble to guarantee a mismatch against the real binary.
	bad := sha[:len(sha)-1] + map[bool]string{true: "0", false: "1"}[sha[len(sha)-1] != '0']

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("example", "broker", config.PluginSpec{Impl: "example", Path: bin, SHA: bad}, "", nil)
	if err == nil {
		t.Fatal("launch with a mismatched sha should refuse, got nil error")
	}
	if h != nil {
		t.Errorf("launch should not return a holder on sha refusal, got %v", h)
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a sha256 mismatch error, got %v", err)
	}
}
