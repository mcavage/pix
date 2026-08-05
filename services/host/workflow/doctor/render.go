package doctor

import "pix/host/health"

// McpHostTrustNotice is the two-fact disclosure for local command/container
// MCP servers: they run on the host, outside sandbox isolation, with your
// host-user privileges, and anything they return can end up in the
// conversation sent to your model provider. It survives the readiness
// renderer that used to print it as a footer because setup's completion
// summary still says it, and the two surfaces must say it identically.
const McpHostTrustNotice = health.MCPHostTrustNotice
