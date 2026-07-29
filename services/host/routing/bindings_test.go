package routing

import "testing"

func TestRegistryForBindingsUsesObservedAvailability(t *testing.T) {
	catalog := &Registry{Models: []Model{
		{ID: "lab/alpha", Provider: "lab", Available: true},
		{ID: "lab/beta", Provider: "lab", Available: true},
		{ID: "other/gamma", Provider: "other", Available: false},
	}}
	got := RegistryForBindings(catalog, []Binding{
		{Model: "lab/alpha", Backend: "direct", Available: true},
		{Model: "lab/beta", Backend: "direct", Available: false},
		{Model: "other/gamma", Backend: "gateway", Available: true},
	}, "")
	if !got.Models[0].Available {
		t.Fatal("proven catalog model should be available")
	}
	if got.Models[1].Available {
		t.Fatal("failed probe must make a catalog model unavailable")
	}
	if got.Models[2].Available {
		t.Fatal("a catalog-retired model must not be resurrected by a binding")
	}
	if !catalog.Models[1].Available {
		t.Fatal("filter must not mutate the shipped catalog")
	}
}

func TestMaterializeBindingsDropsUnboundRoutesAndMapsGatewayIDs(t *testing.T) {
	in := CompiledRouting{Routes: map[string]CompiledRoute{
		"code":   {Model: "lab/alpha"},
		"review": {Model: "other/beta"},
	}}
	got := MaterializeBindings(in, []Binding{{Model: "lab/alpha", Backend: "gateway", UpstreamID: "alpha-prod", Available: true}}, "gateway")
	if got.Routes["code"].Model != "gateway/alpha-prod" {
		t.Fatalf("model = %q", got.Routes["code"].Model)
	}
	if _, ok := got.Routes["review"]; ok {
		t.Fatal("route without a callable binding must be removed")
	}
}

func TestRegistryForBindingsEnforcesExclusiveBackend(t *testing.T) {
	catalog := &Registry{Models: []Model{{ID: "lab/alpha", Available: true}, {ID: "lab/beta", Available: true}}}
	got := RegistryForBindings(catalog, []Binding{
		{Model: "lab/alpha", Backend: "direct", Available: true},
		{Model: "lab/beta", Backend: "work", Available: true},
	}, "work")
	if got.Models[0].Available || !got.Models[1].Available {
		t.Fatalf("exclusive availability = [%v %v], want [false true]", got.Models[0].Available, got.Models[1].Available)
	}
}
