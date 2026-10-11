package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"nyx/internal/feed"
	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
	"nyx/internal/tool"

	"go.opentelemetry.io/otel/trace"
)

// Domain sentinels for the MCP layer. Registered with api.RegisterSentinel
// in init() so the project's centralised error funnel (api.MapError,
// api.WriteError) recognises them when a future HTTP surface reuses
// these types. The MCP layer itself does NOT call api.MapError — it
// renders the error into a JSON-RPC envelope directly because the
// agent is a JSON-RPC client, not a browser.
var (
	ErrInvalidParam = errors.New("mcp: invalid parameter")
	ErrNotFound     = errors.New("mcp: resource not found")
	ErrInternal     = errors.New("mcp: internal error")
)

func init() {
	api.RegisterSentinel(ErrInvalidParam, http.StatusBadRequest, "Invalid parameter")
	api.RegisterSentinel(ErrNotFound, http.StatusNotFound, "Resource not found")
	api.RegisterSentinel(ErrInternal, http.StatusInternalServerError, "Internal error")
}

// Service is the MCP-side projection over the registry. After the
// agent-loop migration the MCP layer owns nothing but the wire
// shape (JSON-RPC envelopes, mcp-go schemas, CallToolResult
// encoding) — every tool definition lives in internal/tool and the
// dispatch path goes Registry.Dispatch → feed.Service.
//
// RegisterTools (called by NewHandler) and the handleXxx shims
// below exist so the existing mcp service tests keep compiling and
// passing against the registry-backed handler without
// modification. The shims are tiny: open a span, call
// registry.Dispatch, translate the registry error into the MCP
// wire form (sentinel → wire sentinel; everything else →
// ErrInternal with the original logged via api.ClassifyAndLog).
type Service struct {
	feeds    feed.Service
	tracer   trace.Tracer
	registry *tool.Registry
}

// NewService wires the MCP service over the existing feed.Service
// AND the registry that owns the tool bodies. The registry is
// created here so RegisterTools / handleXxx / NewHandler all share
// one instance; tests that call NewService directly get the same
// wiring.
func NewService(feeds feed.Service, tracer trace.Tracer) *Service {
	reg := tool.New()
	tool.RegisterFeedTools(reg, feeds)
	return &Service{feeds: feeds, tracer: tracer, registry: reg}
}

// Registry exposes the registry for tests / future consumers that
// want to inspect what the MCP surface is actually serving. The
// primary consumer is NewHandler's tools/list projection.
func (s *Service) Registry() *tool.Registry { return s.registry }

// RegisterTools attaches the feed tools to the supplied MCP
// server. Each tool's input schema is built via the fluent builder
// (mcp.WithString / WithNumber / etc.) so the JSON Schema clients
// receive is explicit and version-controlled.
//
// The wire schema here is the only place the MCP layer duplicates
// metadata — but that duplication is the WHOLE POINT of the MCP
// surface (the JSON-Schema the agent client receives). The
// dispatcher body, arg validation, and per-owner scoping live in
// internal/tool — see RegisterFeedTools.
func (s *Service) RegisterTools(mcpServer *server.MCPServer) {
	mcpServer.AddTool(
		mcp.NewTool(ToolListFeeds,
			mcp.WithDescription("List the authenticated caller's feeds. Supports search (max 200 chars), page (min 1, default 1), page_size (clamped to 100, default 20), and order (asc|desc, default desc). Returns the standard pagination envelope: {data, page, page_size, total, has_more}."),
			mcp.WithString("query",
				mcp.Description("Case-insensitive search term matched against title and description."),
				mcp.MaxLength(200),
			),
			mcp.WithNumber("page",
				mcp.Description("1-based page index."),
				mcp.Min(1),
			),
			mcp.WithNumber("page_size",
				mcp.Description("Items per page; clamped server-side to 100."),
				mcp.Min(1),
			),
			mcp.WithString("order",
				mcp.Description("Sort direction: 'desc' returns newest first (default), 'asc' returns oldest first."),
				mcp.Enum("asc", "desc"),
			),
		),
		mcp.NewTypedToolHandler(s.handleListFeeds),
	)

	mcpServer.AddTool(
		mcp.NewTool(ToolGetFeed,
			mcp.WithDescription("Read a single feed by id. Returns an error result when the feed does not exist OR is owned by another user (single sentinel, no existence leak)."),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("Feed id."),
				mcp.Min(1),
			),
		),
		mcp.NewTypedToolHandler(s.handleGetFeed),
	)

	mcpServer.AddTool(
		mcp.NewTool(ToolCreateFeed,
			mcp.WithDescription("Create a new feed. title is required (1-100 chars); description is optional (max 1000 chars). Returns the created feed (with server-generated id, created_at, updated_at)."),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Title of the feed."),
				mcp.MinLength(1),
				mcp.MaxLength(100),
			),
			mcp.WithString("description",
				mcp.Description("Optional description."),
				mcp.MaxLength(1000),
			),
		),
		mcp.NewTypedToolHandler(s.handleCreateFeed),
	)

	mcpServer.AddTool(
		mcp.NewTool(ToolUpdateFeed,
			mcp.WithDescription("Update an existing feed. id is required; title is required (1-100 chars); description is optional (max 1000 chars). Returns an error result if the feed does not exist OR is owned by another user."),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("Feed id."),
				mcp.Min(1),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Title of the feed."),
				mcp.MinLength(1),
				mcp.MaxLength(100),
			),
			mcp.WithString("description",
				mcp.Description("Optional description."),
				mcp.MaxLength(1000),
			),
		),
		mcp.NewTypedToolHandler(s.handleUpdateFeed),
	)

	mcpServer.AddTool(
		mcp.NewTool(ToolDeleteFeed,
			mcp.WithDescription("Soft-delete a feed by id. Returns an error result if the feed does not exist OR is owned by another user."),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("Feed id."),
				mcp.Min(1),
			),
		),
		mcp.NewTypedToolHandler(s.handleDeleteFeed),
	)
}

// ---- Typed argument structs ----
//
// The mcp-go typed handler unmarshals wire arguments into one of
// these per call. The JSON tags drive wire field naming; the
// matching mcp.WithString/WithNumber etc. in RegisterTools above
// drive the input schema clients see. The names must match —
// mcp-go parses arguments into the typed struct by JSON key.
//
// These are TYPE ALIASES for the canonical definitions in
// internal/tool/feeds.go. The alias lets the existing service tests
// reference the lowercase names they were written against (e.g.
// listFeedsArgs) without an import change while the canonical type
// lives next to the tool dispatcher that consumes it.

type (
	listFeedsArgs  = tool.ListFeedsArgs
	getFeedArgs    = tool.GetFeedArgs
	createFeedArgs = tool.CreateFeedArgs
	updateFeedArgs = tool.UpdateFeedArgs
	deleteFeedArgs = tool.DeleteFeedArgs
)

// ---- Per-tool handlers ----
//
// Each handler is a thin shim: open an OTel span, call
// registry.Dispatch, translate the returned error into the MCP wire
// form, marshal the result as CallToolResult. All the actual
// business logic — including arg normalisation, per-owner scoping,
// validation, and safe-error mapping — runs inside the registry
// tool body. This keeps the MCP layer a pure wire shim.

func (s *Service) handleListFeeds(ctx context.Context, _ mcp.CallToolRequest, args listFeedsArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.list_feeds", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	result, err := s.registry.Dispatch(ctx, ToolListFeeds, args)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to list feeds")
	}
	return feedResult(result)
}

func (s *Service) handleGetFeed(ctx context.Context, _ mcp.CallToolRequest, args getFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.get_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	result, err := s.registry.Dispatch(ctx, ToolGetFeed, args)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to get feed")
	}
	return feedResult(result)
}

func (s *Service) handleCreateFeed(ctx context.Context, _ mcp.CallToolRequest, args createFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.create_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	// Pre-migration write behaviour: missing user id fails closed
	// with ErrNotFound, never ErrInvalidParams — single sentinel,
	// no existence leak. The auth middleware in production stamps
	// the id before this handler runs; the test that exercises the
	// no-id path is TestService_CreateFeed_NoUserID_FailsClosed.
	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return nil, ErrNotFound
	}

	result, err := s.registry.Dispatch(ctx, ToolCreateFeed, args)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to create feed")
	}
	return feedResult(result)
}

func (s *Service) handleUpdateFeed(ctx context.Context, _ mcp.CallToolRequest, args updateFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.update_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return nil, ErrNotFound
	}

	result, err := s.registry.Dispatch(ctx, ToolUpdateFeed, args)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to update feed")
	}
	return feedResult(result)
}

func (s *Service) handleDeleteFeed(ctx context.Context, _ mcp.CallToolRequest, args deleteFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.delete_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return nil, ErrNotFound
	}

	result, err := s.registry.Dispatch(ctx, ToolDeleteFeed, args)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to delete feed")
	}
	raw, _ := json.Marshal(result)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.NewTextContent(string(raw))},
		StructuredContent: result,
	}, nil
}

// synthOwnerForRead was a fail-open fallback that stamped a synthetic user id when the
// context carried none. Removed per agent-langgraph review (MEDIUM-9) — reads now fail
// closed: a missing user id returns ErrMissingOwner from the registry instead of being
// silently synthesized as user 1.

// feedResult wraps a payload as both a JSON text content item AND a
// structured-content object so clients that understand the typed
// shape get typed data and clients that don't get a JSON string. See
// the MCP spec's "structured content" guidance — agents prefer typed
// data when available.
func feedResult(payload any) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal tool result: %w", err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.NewTextContent(string(raw))},
		StructuredContent: payload,
	}, nil
}

// mapSentinel translates the registry's error space into the
// MCP-layer wire sentinels. Order matters: the registry's
// ErrMissingOwner (and ErrInvalidArgs) are fail-closed guards that
// must surface as a usable wire error — they're treated like
// sentinel errors here (missing owner → ErrNotFound matches the
// pre-migration behaviour; invalid args → ErrInvalidParam gives
// the agent a typed "your request was malformed" response).
//
// Everything else (panic, internal, unknown wrapped errors) lands
// on ErrInternal with the underlying error logged via
// api.ClassifyAndLog — err.Error() never reaches the wire.
//
// safeDetail is the static literal api.ClassifyAndLog writes to the
// wire envelope on the unknown-error path; the caller's mapping
// never copies err.Error() into the response.
func mapSentinel(ctx context.Context, err error, safeDetail string) error {
	switch {
	case errors.Is(err, feed.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, tool.ErrMissingOwner):
		// A tool whose body required an owner but was dispatched
		// without one must fail closed with the same wire form
		// the pre-migration handler used (ErrNotFound — a
		// single sentinel, no existence leak).
		return ErrNotFound
	case errors.Is(err, tool.ErrInvalidArgs):
		return ErrInvalidParam
	case errors.Is(err, tool.ErrToolNotFound):
		return ErrInvalidParam
	case errors.Is(err, tool.ErrPanic):
		api.ClassifyAndLog(ctx, err, safeDetail)
		return ErrInternal
	case errors.Is(err, tool.ErrInternal):
		api.ClassifyAndLog(ctx, err, safeDetail)
		return ErrInternal
	}
	// Unclassified error from a tool body. Log via the safe-error
	// funnel and render as ErrInternal — never copy err.Error()
	// to the wire.
	api.ClassifyAndLog(ctx, err, safeDetail)
	return ErrInternal
}

// reqctx import removed: the registry owns the per-owner scoping
// now. Service callers (mcp.Handler) still touch the user id via
// middleware before reaching this layer, so we never need the
// typed ctx helper here.
