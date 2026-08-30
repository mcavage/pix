package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeChildRequest_RefusesGeneralCommand(t *testing.T) {
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process","command":"rm -rf /"}`)
	if _, err := DecodeChildRequest(raw); err == nil {
		t.Fatal("expected a refusal for a field outside the bounded contract, got nil")
	}
}

func TestDecodeChildRequest_RefusesArgv(t *testing.T) {
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","target":"local-process","argv":["sh","-c","echo hi"]}`)
	if _, err := DecodeChildRequest(raw); err == nil {
		t.Fatal("expected a refusal for argv, got nil")
	}
}

func TestDecodeChildRequest_AcceptsBoundedFields(t *testing.T) {
	raw := json.RawMessage(`{"agent":"fanout","task":"do it","model":"anthropic/claude-sonnet-5","target":"local-process"}`)
	req, err := DecodeChildRequest(raw)
	if err != nil {
		t.Fatalf("DecodeChildRequest: %v", err)
	}
	if req.Agent != "fanout" || req.Task != "do it" || req.Target != "local-process" {
		t.Fatalf("decoded %+v", req)
	}
}

func TestChildRequestValidate_RequiresFields(t *testing.T) {
	cases := []ChildRequest{
		{Task: "t", Target: "local-process"},
		{Agent: "a", Target: "local-process"},
		{Agent: "a", Task: "t"},
	}
	for _, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("expected an error for %+v", c)
		}
	}
}

func TestChildRequestValidate_RefusesUnknownTarget(t *testing.T) {
	req := ChildRequest{Agent: "a", Task: "t", Target: "quantum-cloud"}
	err := req.Validate()
	var unknown *UnknownTargetError
	if !errors.As(err, &unknown) {
		t.Fatalf("Validate() = %v, want *UnknownTargetError", err)
	}
}

func TestChildRequestValidate_KnownButUnsupportedPassesValidate(t *testing.T) {
	// Validate() only checks the value is a SCHEMA-known target; capability
	// (Supported()) is a distinct check made separately via CheckTarget, so a
	// caller can distinguish "not a target" from "not yet".
	req := ChildRequest{Agent: "a", Task: "t", Target: "local-sandbox"}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (local-sandbox is schema-known)", err)
	}
	if err := CheckTarget(Target(req.Target)); err == nil {
		t.Fatal("CheckTarget(local-sandbox) = nil, want an UnsupportedTargetError")
	} else if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("CheckTarget error = %v, want an unsupported-capability message", err)
	}
}
