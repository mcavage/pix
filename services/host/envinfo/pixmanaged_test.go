package envinfo

import "testing"

// A Pix-managed reference resolves to the home Pix itself resolved, and a
// HOST reference is left exactly as authored for sbx: expanding both here
// would push a possibly credential-shaped host value into the persisted
// effective document, which is the whole reason this expansion is narrow.
func TestExpandPixManaged_OnlyPixOwnedNames(t *testing.T) {
	vars := PixManagedVars("/Users/someone/.pix")
	cases := []struct{ in, want string }{
		{"${PIX_HOME}/.state/integrations/gog:/home/gog", "/Users/someone/.pix/.state/integrations/gog:/home/gog"},
		{"-v", "-v"},
		{"${GOG_KEYRING_PASSWORD}", "${GOG_KEYRING_PASSWORD}"},
		{"${PIX_HOME:-/nope}/x", "/Users/someone/.pix/x"},
		{"${PIX_HOME}:${OTHER}", "/Users/someone/.pix:${OTHER}"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ExpandPixManaged(c.in, vars); got != c.want {
			t.Fatalf("ExpandPixManaged(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An unresolvable home defines NOTHING. `${PIX_HOME}` must then stay
// undefined — and be refused by RefuseUndefinedInterpolations — rather than
// expand to "", which would mount the filesystem root into a container.
func TestPixManagedVars_UnresolvedHomeDefinesNothing(t *testing.T) {
	for _, home := range []string{"", "   "} {
		vars := PixManagedVars(home)
		if len(vars) != 0 {
			t.Fatalf("PixManagedVars(%q) = %v, want no variables", home, vars)
		}
		if got := ExpandPixManaged("${PIX_HOME}/x", vars); got != "${PIX_HOME}/x" {
			t.Fatalf("expanded against an unresolved home: %q", got)
		}
	}
}

// The undefined-variable refusal and the argv expansion must agree on the
// SAME set of names, or one refuses an environment the other accepted.
func TestLookupPixManaged_LayersOverTheHostProbe(t *testing.T) {
	base := func(name string) (string, bool) {
		switch name {
		case "PIX_HOME":
			return "/some/other/home", true
		case "HOST_ONLY":
			return "host-value", true
		}
		return "", false
	}
	lookup := LookupPixManaged(PixManagedVars("/pix/home"), base)

	if v, ok := lookup("PIX_HOME"); !ok || v != "/pix/home" {
		t.Fatalf("PIX_HOME = %q,%v; Pix's own home must win over the host's", v, ok)
	}
	if v, ok := lookup("HOST_ONLY"); !ok || v != "host-value" {
		t.Fatalf("HOST_ONLY = %q,%v; every other name must fall through", v, ok)
	}
	if _, ok := lookup("NOT_SET"); ok {
		t.Fatal("NOT_SET resolved; an unset host variable must stay undefined")
	}

	tree := &Tree{Interpolations: []Interpolation{{Var: "PIX_HOME", KeyPath: "mcp.servers[gog].args[7]"}}}
	if err := RefuseUndefinedInterpolations(tree, lookup); err != nil {
		t.Fatalf("an authored ${PIX_HOME} was refused as undefined: %v", err)
	}
	if err := RefuseUndefinedInterpolations(tree, nil); err == nil {
		t.Fatal("without the Pix-managed layer ${PIX_HOME} must still be refused")
	}
}

func TestExpandPixManagedArgv_ExpandsEveryElement(t *testing.T) {
	vars := PixManagedVars("/pix/home")
	in := []string{"docker", "run", "-v", "${PIX_HOME}/.state/integrations/slack:/home/nonroot", "img"}
	got := ExpandPixManagedArgv(in, vars)
	want := "/pix/home/.state/integrations/slack:/home/nonroot"
	if got[3] != want {
		t.Fatalf("argv[3] = %q, want %q", got[3], want)
	}
	if in[3] == got[3] {
		t.Fatal("ExpandPixManagedArgv mutated the caller's slice")
	}
}
