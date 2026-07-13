package plugin

import "encoding/json"

// The three capability interfaces below are derived from the real host services.
// They deliberately use plain Go structs (no context.Context, no maps of any) so
// their arguments and results are trivially gob-compatible for the net/rpc
// transport. When the transport is upgraded to gRPC, context can be threaded
// back in per the arch doc.

// --- MemoryStore -------------------------------------------------------------
//
// Mirrors the JSON-RPC surface of ../memory.go (the `methods` map in
// memoryMux(): remember, recall, forget, synthesize, promotable, observe, stats,
// health). The kernel keeps the JSON-RPC front-end on :11435 and turns each
// method into a typed call on this interface.

type MemoryStore interface {
	Remember(RememberReq) (RememberResp, error)
	Recall(RecallReq) (RecallResp, error)
	Forget(ForgetReq) (ForgetResp, error)
	Synthesize(SynthesizeReq) (SynthesizeResp, error)
	Promotable(PromotableReq) (PromotableResp, error)
	Observe(ObserveReq) (ObserveResp, error)
	Stats() (Stats, error)
	Health() (Health, error)
}

// RememberReq mirrors memory.go's rememberInput / rememberFromParams.
type RememberReq struct {
	Content    string
	Kind       string
	Durability string
	Source     string
	Project    string
	HasProject bool
	TTLDays    int
	Confidence float64
	Reward     float64
	Tags       []string
	Dedupe     float64
	HasDedupe  bool
}

// RememberResp mirrors memory.go remember()'s {id, reaffirmed}.
type RememberResp struct {
	ID         string
	Reaffirmed bool
}

// RecallReq mirrors the recall() params.
type RecallReq struct {
	Query      string
	Limit      int
	CharBudget int
	Kind       string
	Project    string
}

// Hit mirrors a scoredHit as surfaced by the recall JSON-RPC result.
type Hit struct {
	ID         string
	Content    string
	Score      float64
	Kind       string
	Durability string
	Project    string
}

// RecallResp wraps the hit list ({"hits": [...]}).
type RecallResp struct {
	Hits []Hit
}

// ForgetReq / ForgetResp mirror forget(idOrPrefix) -> {ok}.
type ForgetReq struct {
	ID string
}

type ForgetResp struct {
	OK bool
}

// SynthesizeReq / SynthesizeResp mirror synthesize(threshold) -> {merged, expired}.
type SynthesizeReq struct {
	Threshold float64
}

type SynthesizeResp struct {
	Merged  int
	Expired int64
}

// PromotableReq / PromotableResp mirror promotable(minFrequency) -> {candidates}.
type PromotableReq struct {
	MinFrequency int
}

type Candidate struct {
	ID        string
	Content   string
	Frequency int
	Project   string
}

type PromotableResp struct {
	Candidates []Candidate
}

// ObserveReq / ObserveResp mirror the observe method (async memCapture).
type ObserveReq struct {
	User       string
	Project    string
	HasProject bool
}

type ObserveResp struct {
	Accepted bool
	Reason   string
}

// Stats mirrors stats().
type Stats struct {
	Active     int
	Durable    int
	Perishable int
	Facts      int
	Learnings  int
	Deleted    int
}

// Health mirrors the health method ({ok, vector, capture, watcherModel}).
type Health struct {
	OK           bool
	Vector       bool
	Capture      bool
	WatcherModel string
}

// --- KnowledgeStore ----------------------------------------------------------
//
// A retrieval-augmented knowledge base over one or more concept "bundles".
// Query returns cited concepts ranked by relevance; Reindex (re)ingests bundle
// paths; Health reports index status. Like MemoryStore, it uses plain Go structs
// so arguments and results are trivially gob-compatible for the net/rpc
// transport.

type KnowledgeStore interface {
	Query(QueryArgs) (QueryResult, error)
	Reindex(ReindexArgs) (ReindexResult, error)
	Health() (KnowledgeHealth, error)
}

// QueryArgs parameterizes a knowledge query. An empty Bundle means all bundles.
type QueryArgs struct {
	Query  string
	Bundle string
	Limit  int
}

// CitedConcept is a single ranked, cited result concept.
type CitedConcept struct {
	ID          string
	Type        string
	Title       string
	Description string
	Path        string
	Snippet     string
	Score       float64
	Citations   []string
	Bundle      string
}

// QueryResult wraps the ranked concept list.
type QueryResult struct {
	Concepts []CitedConcept
}

// ReindexArgs lists the bundle paths to (re)index.
type ReindexArgs struct {
	BundlePaths []string
}

// ReindexResult reports the number of concepts indexed and the bundles touched.
type ReindexResult struct {
	Indexed int
	Bundles []string
}

// KnowledgeHealth reports index status ({ok, vector, bundles, concepts}).
type KnowledgeHealth struct {
	OK       bool
	Vector   bool
	Bundles  []string
	Concepts int
}

// --- CredentialBroker --------------------------------------------------------
//
// Generalizes ../gwstoken.go's mint/check. The "keep the long-lived credential
// on the host, mint a short-lived one, run the real CLI in the VM" pattern
// becomes one interface any provider can implement. gws ignores Audience today;
// a warehouse/CRM broker would use it.

type CredentialBroker interface {
	Mint(audience string, scopes []string) (Token, error)
	Check() error
	Describe() (BrokerInfo, error)
}

// Token mirrors gwstoken.go's gwsBearer.
type Token struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int
}

// BrokerInfo describes a broker to the supervisor (port, auth header shape,
// whether it shells out to a host CLI as gws does).
type BrokerInfo struct {
	Name            string
	DefaultPort     int
	AuthHeader      string
	RequiresHostCLI bool
}

// --- McpServer ---------------------------------------------------------------
//
// Mirrors ../slack.go + the MCP scaffolding in ../util.go (mcpDispatcher:
// initialize / tools/list / tools/call). The registered sbx-gateway stdio
// command becomes a thin compiled bridge that forwards ListTools/CallTool to an
// McpServer plugin, keeping the "compiled Go spawns from network input" EDR
// property the arch doc requires.

type McpServer interface {
	Info() (ServerInfo, error)
	ListTools() ([]ToolSpec, error)
	CallTool(name string, args json.RawMessage) (json.RawMessage, error)
}

// ServerInfo mirrors the initialize result's serverInfo + protocolVersion.
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
}

// ToolSpec mirrors util.go's mcpTool.schema() output. InputSchema is carried as
// raw JSON so arbitrary schemas stay gob-compatible.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}
