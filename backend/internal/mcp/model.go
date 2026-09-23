// Package mcp exposes the feed domain to external AI agents over the
// Model Context Protocol (Streamable HTTP transport, JSON-RPC 2.0).
//
// The server is opt-in via MCP_ENABLED; when disabled nothing is
// constructed and the router carries no /mcp route. When enabled,
// the JWT bearer middleware runs before the route handler (same as
// /api/chat) so every tool call is per-owner scoped via the
// auth-stamped context — cross-owner reads bubble up as feed.ErrNotFound
// (single sentinel, no existence leak).
//
// Tools only — no Resources, no Prompts. See FUTURE_BACKEND.md §11.
package mcp

// MCP method names (JSON-RPC 2.0 method field). Pinned as constants
// so a typo at a call site fails to compile rather than silently
// producing a -32601 MethodNotFound at runtime.
const (
	MethodInitialize = "initialize"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"
	MethodPing       = "ping"
)

// Tool names — snake_case per MCP convention. The agent-facing
// display labels live on the tool's Description in service.go.
const (
	ToolListFeeds  = "list_feeds"
	ToolGetFeed    = "get_feed"
	ToolCreateFeed = "create_feed"
	ToolUpdateFeed = "update_feed"
	ToolDeleteFeed = "delete_feed"
)

// maxPageSize is the hard upper bound on page_size for the list_feeds
// tool. Matches the REST cap at feed/huma_handler.go:258-260 so the
// two surfaces cannot diverge.
const maxPageSize = 100
