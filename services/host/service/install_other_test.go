//go:build !darwin

package service

import "testing"

// TestUnsupportedHostStub proves the non-darwin managed-service stub has NO
// lifecycle behavior: every entry point degrades to the single sentinel
// error, nothing is written, nothing is spawned.
func TestUnsupportedHostStub(t *testing.T) {
	if err := platformServeInstall(nil); err != ErrUnsupportedHost {
		t.Errorf("platformServeInstall = %v, want ErrUnsupportedHost", err)
	}
	if err := platformServeUninstall(nil); err != ErrUnsupportedHost {
		t.Errorf("platformServeUninstall = %v, want ErrUnsupportedHost", err)
	}
	if ManagedActive() {
		t.Error("ManagedActive must always report false on an unsupported host")
	}
	if err := restartManagedService(); err != ErrUnsupportedHost {
		t.Errorf("restartManagedService = %v, want ErrUnsupportedHost", err)
	}
	if err := StopManaged(nil); err != ErrUnsupportedHost {
		t.Errorf("StopManaged = %v, want ErrUnsupportedHost", err)
	}
}
