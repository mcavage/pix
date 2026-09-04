package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ChildRequest is the bounded, structured request the session-control MCP
// tool accepts (architecture §7.2: "not a general shell or plugin API").
// Deliberately, the ONLY fields this type carries are agent/task/model/
// target: there is no argv, no command, no shell field anywhere in the
// contract, so a caller cannot smuggle an arbitrary host invocation through
// it no matter what strings it puts in Agent/Task/Model.
type ChildRequest struct {
	Agent  string `json:"agent"`
	Task   string `json:"task"`
	Model  string `json:"model,omitempty"`
	Target string `json:"target"`
}

// DecodeChildRequest parses raw as a ChildRequest and refuses any field the
// contract does not name (DisallowUnknownFields): a request that also sets
// "command"/"argv"/"shell" is a schema violation, not a request this build
// silently drops fields from.
func DecodeChildRequest(raw json.RawMessage) (ChildRequest, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var r ChildRequest
	if err := dec.Decode(&r); err != nil {
		return ChildRequest{}, fmt.Errorf("pix: session request does not match the bounded contract (agent/task/model/target only): %w", err)
	}
	return r, nil
}

// Validate checks the request is well-formed and names a target this
// contract knows. It does NOT check Supported() — capability refusal is a
// distinct, differently-worded error (UnsupportedTargetError) from a
// missing-field schema error, and callers that need both consult
// session.CheckTarget separately (see NewChildNode).
func (r ChildRequest) Validate() error {
	if strings.TrimSpace(r.Agent) == "" {
		return fmt.Errorf("pix: session request requires a non-empty agent")
	}
	if strings.TrimSpace(r.Task) == "" {
		return fmt.Errorf("pix: session request requires a non-empty task")
	}
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("pix: session request requires a non-empty target")
	}
	if !Target(r.Target).Known() {
		return &UnknownTargetError{Value: r.Target}
	}
	return nil
}
