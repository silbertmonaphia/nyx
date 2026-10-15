package mcp

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mark3labs/mcp-go/server"

	"nyx/internal/middleware"
	"nyx/internal/platform/auth"
)

// JSON-RPC 2.0 error codes used by the MCP layer. The negative
// integers in the -326xx range are spec-defined (mcp-go emits them
// automatically for bad JSON / unknown methods / invalid params);
// -32004 / -32005 are reserved for application-level codes (feed
// not found, rate-limited) so clients can distinguish "the server
// doesn't know this resource" from "your app data isn't there".
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
	rpcAppNotFound    = -32004
	rpcAppRateLimited = -32005
)

// jsonRPCError is the {code, message, data} shape emitted in the
// "error" field of a JSON-RPC 2.0 response. mcp-go handles most
// envelope plumbing internally; this struct is used only by helpers
// that synthesise responses from outside mcp-go (the rate-limit
// middleware short-circuit).
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// jsonRPCResponse is the wire envelope emitted when we synthesise
// a JSON-RPC response outside mcp-go (currently only the rate-limit
// path). Mirrors the structure mcp-go produces for genuine tool
// failures so the client sees one consistent envelope shape.
type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

// Handler wraps an mcp-go StreamableHTTPServer so it can be mounted
// on chi as an http.Handler. Construction is split from RegisterMCPRoute
// so tests can build a Handler without spinning up the full main.go
// wiring.
type Handler struct {
	path         string
	maxBodyBytes int64
	streamable   *server.StreamableHTTPServer
}

// NewHandler builds the Streamable HTTP server with the feed tools
// attached. Stateless mode keeps every request self-contained: the
// JWT in the Authorization header is the only session identifier
// the server tracks, so a request without a valid bearer 401s on
// the chi middleware before this handler is ever invoked.
//
// DisableLocalhostProtection is intentional: the route sits behind
// chi + the project's standard middleware chain, which is the trust
// boundary. The mcp-go localhost guard would block browser clients
// running on localhost, which is the wrong default for an
// HTTP-fronted MCP server.
func NewHandler(svc *Service, path string, maxBodyBytes int64) *Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 65536
	}
	mcpServer := server.NewMCPServer(
		"nyx-feeds",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
		// server.WithInputSchemaValidation enforces our declared
		// mcp.NewTool schemas (Required, Min, MaxLength, Enum) on
		// incoming arguments. Without it the schema is purely
		// advisory — clients see the constraints but a hostile
		// agent can send any JSON. Defence-in-depth: reject
		// invalid args at the framework boundary before the typed
		// handler runs. Per [SEP-1303] the failure surfaces as a
		// tool-level IsError result with a message the LLM can
		// self-correct on, not as a JSON-RPC 401-style error.
		server.WithInputSchemaValidation(),
	)
	svc.RegisterTools(mcpServer)

	streamable := server.NewStreamableHTTPServer(
		mcpServer,
		server.WithEndpointPath(path),
		server.WithStateLess(true),
		server.WithDisableLocalhostProtection(true),
	)

	return &Handler{
		path:         path,
		maxBodyBytes: maxBodyBytes,
		streamable:   streamable,
	}
}

// RegisterMCPRoute mounts the MCP route on chi. Mirrors
// chat.RegisterChatRoute exactly:
//   - middleware.NewAuth runs first to stamp userID on the context;
//   - the per-user limiter (when non-nil) runs next;
//   - the StreamableHTTPServer is mounted as the route's terminal
//     handler.
//
// The MCP_ENABLED toggle lives in cmd/api/main.go (this function
// is only called when the flag is on), so a disabled deployment
// pays no allocation.
func RegisterMCPRoute(router chi.Router, h *Handler, tokens auth.TokenService, limiter *UserRateLimiter) {
	if h == nil {
		panic("mcp.RegisterMCPRoute: handler is required")
	}
	router.Route(h.path, func(r chi.Router) {
		r.Use(middleware.NewAuth(tokens))
		if limiter != nil {
			r.Use(limiter.Middleware)
		}
		r.Handle("/", h)
	})
}

// ServeHTTP wraps the StreamableHTTPServer with an inner per-body
// cap so an oversized JSON-RPC payload fails fast. The outer
// router-level 1 MiB cap (cmd/api/main.go maxBodyBytesLimit) still
// fires first on truly huge bodies; this is the inner belt-and-
// braces for the MCP-specific size budget (default 64 KiB).
//
// mcp-go's StreamableHTTPServer implements http.Handler; this method
// makes Handler itself an http.Handler so chi can mount it directly.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil && r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	}
	h.streamable.ServeHTTP(w, r)
}
