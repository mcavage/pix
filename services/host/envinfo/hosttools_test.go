package envinfo

import "testing"

func TestHostToolsAuthorityIsPartOfRegistration(t *testing.T) {
	base := BuiltinMCPFacts{SessionCommand: "/bin/pix"}
	normal := WithHostTools(base, "/home/me/.pix", "/workspace", "pix-sandbox", false, false)
	dev := WithHostTools(base, "/home/me/.pix", "/workspace", "pix-sandbox", true, false)
	other := WithHostTools(base, "/home/me/.pix", "/other", "pix-sandbox", true, false)
	if normal.SessionName == dev.SessionName || dev.SessionName == other.SessionName {
		t.Fatal("registrations share authority")
	}
	for _, f := range []BuiltinMCPFacts{normal, dev, other} {
		if !IsReservedMCPName(f.SessionName) {
			t.Fatalf("unreserved %s", f.SessionName)
		}
		if f.SessionArgs[0] != HostToolsSubcommand {
			t.Fatal("not routed to host tools")
		}
	}
	disabled := WithHostTools(base, "/home/me/.pix", "/workspace", "pix-sandbox", true, true)
	if len(BuiltinMCPServers(disabled)) != 0 {
		t.Fatal("trial regained host tools")
	}
}
