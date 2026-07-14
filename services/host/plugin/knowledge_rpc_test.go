package plugin

import (
	"errors"
	"strings"
	"testing"
)

// echoKnowledge returns distinctive, argument-derived values so a round trip can
// be asserted end to end (not just non-error), mirroring echoMemory.
type echoKnowledge struct{}

func (echoKnowledge) Query(a QueryArgs) (QueryResult, error) {
	return QueryResult{Concepts: []CitedConcept{{
		ID:          "c1",
		Type:        "concept",
		Title:       a.Query,
		Description: "desc",
		Path:        "bundles/x.md",
		Snippet:     "snip",
		Score:       0.75,
		Citations:   []string{"src1", "src2"},
		Bundle:      strings.Join(a.Bundles, ","),
	}}}, nil
}
func (echoKnowledge) Reindex(a ReindexArgs) (ReindexResult, error) {
	return ReindexResult{Indexed: len(a.BundlePaths), Bundles: a.BundlePaths}, nil
}
func (echoKnowledge) Health() (KnowledgeHealth, error) {
	return KnowledgeHealth{OK: true, Vector: true, Bundles: []string{"core"}, Concepts: 42}, nil
}

func TestRPCRoundTripKnowledge(t *testing.T) {
	client := newRPCPair(t, &knowledgeRPCServer{Impl: echoKnowledge{}})
	c := &knowledgeRPCClient{client: client}

	got, err := c.Query(QueryArgs{Query: "how", Bundles: []string{"core"}, Limit: 5})
	if err != nil || len(got.Concepts) != 1 {
		t.Fatalf("Query round trip: got %+v err %v", got, err)
	}
	con := got.Concepts[0]
	if con.Title != "how" || con.Bundle != "core" || con.Score != 0.75 {
		t.Fatalf("Query concept fields: got %+v", con)
	}
	if len(con.Citations) != 2 || con.Citations[0] != "src1" || con.Citations[1] != "src2" {
		t.Fatalf("Query citations slice did not round trip: got %+v", con.Citations)
	}

	r, err := c.Reindex(ReindexArgs{BundlePaths: []string{"a", "b", "c"}})
	if err != nil || r.Indexed != 3 || len(r.Bundles) != 3 || r.Bundles[2] != "c" {
		t.Fatalf("Reindex round trip: got %+v err %v", r, err)
	}

	// Zero-arg method — the exact shape net/rpc would drop with an unexported arg.
	h, err := c.Health()
	if err != nil || !h.OK || !h.Vector || h.Concepts != 42 || len(h.Bundles) != 1 || h.Bundles[0] != "core" {
		t.Fatalf("Health round trip: got %+v err %v", h, err)
	}
}

// errKnowledge returns a distinctive error from the zero-arg Health method so we
// can assert the error string survives the rpc.ServerError round trip.
type errKnowledge struct{ echoKnowledge }

func (errKnowledge) Health() (KnowledgeHealth, error) {
	return KnowledgeHealth{}, errors.New("boom: health failed")
}

func TestRPCRoundTripKnowledgeError(t *testing.T) {
	client := newRPCPair(t, &knowledgeRPCServer{Impl: errKnowledge{}})
	c := &knowledgeRPCClient{client: client}

	_, err := c.Health()
	if err == nil {
		t.Fatal("Health: expected non-nil error from impl, got nil")
	}
	if !strings.Contains(err.Error(), "boom: health failed") {
		t.Fatalf("Health error message not preserved: got %q", err.Error())
	}
}
