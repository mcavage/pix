//go:build !darwin

package service

import "testing"

// TestUnsupportedHostStub proves the non-darwin managed-service stub has NO
// lifecycle behavior: every entry point degrades to the single sentinel
// error, nothing is written, nothing is spawned.
func TestUnsupportedHostStub(t *testing.T) {
	if err := platformServeInstall(nil); err != errUnsupportedHost {
		t.Errorf("platformServeInstall = %v, want errUnsupportedHost", err)
	}
	if err := platformServeUninstall(nil); err != errUnsupportedHost {
		t.Errorf("platformServeUninstall = %v, want errUnsupportedHost", err)
	}
	if ManagedActive() {
		t.Error("ManagedActive must always report false on an unsupported host")
	}
	if err := restartManagedService(); err != errUnsupportedHost {
		t.Errorf("restartManagedService = %v, want errUnsupportedHost", err)
	}
	if err := StopManaged(nil); err != errUnsupportedHost {
		t.Errorf("StopManaged = %v, want errUnsupportedHost", err)
	}
}
