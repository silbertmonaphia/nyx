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

// Service is the MCP-side projection of feed.Service. It owns:
//   - the mcp.NewTool input schemas (separate from the typed handler
//     structs so the wire format clients see is decoupled from the
//     internal Go shape);
//   - per-tool handler functions that translate MCP tool args into
//     feed.Service calls, opening one OTel span per dispatch;
//   - safe-error mapping: feed.ErrNotFound → ErrNotFound (so the
//     handler can render it as an MCP tool error), everything else →
//     ErrInternal (with the underlying error logged via
//     api.ClassifyAndLog so err.Error() never reaches the wire).
type Service struct {
	feeds  feed.Service
	tracer trace.Tracer
}

// NewService wires the MCP service over the existing feed.Service.
// The tracer is the noop tracer when OTEL_ENABLED=false so spans
// are free.
func NewService(feeds feed.Service, tracer trace.Tracer) *Service {
	return &Service{feeds: feeds, tracer: tracer}
}

// RegisterTools attaches the five feed tools to the supplied MCP
// server. Each tool's input schema is built via the fluent builder
// (mcp.WithString / WithNumber / etc.) so the JSON Schema clients
// receive is explicit and version-controlled.
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
// mcp.NewTypedToolHandler unmarshals the wire "arguments" object into
// one of these per call. JSON tags drive field naming; the matching
// mcp.WithString/WithNumber etc. in RegisterTools above drive the
// input schema clients see.
//
// The names must match the schema property names — mcp-go parses
// arguments into the typed struct by JSON key.

type listFeedsArgs struct {
	Query    string `json:"query,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
	Order    string `json:"order,omitempty"`
}

type getFeedArgs struct {
	ID int `json:"id"`
}

type createFeedArgs struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type updateFeedArgs struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type deleteFeedArgs struct {
	ID int `json:"id"`
}

// ---- Per-tool handlers ----
//
// Each handler:
//   1. Opens an OTel span (kind=internal, noop when OTEL_ENABLED=false).
//   2. For writes: stamps feed.UserID from reqctx (defensive userID==0
//      guard mirrors feed.Service.GetFeeds — fail-closed rather than
//      letting an unauthenticated context mutate state).
//   3. Calls feed.Service.
//   4. Maps errors via mapSentinel: feed.ErrNotFound → mcp.ErrNotFound
//      (renders as MCP tool error), everything else → api.ClassifyAndLog
//      (logs original error with request id) then mcp.ErrInternal.
//      err.Error() never reaches the wire — the safe-detail contract
//      holds across the MCP surface too.

func (s *Service) handleListFeeds(ctx context.Context, _ mcp.CallToolRequest, args listFeedsArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.list_feeds", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	page := args.Page
	if page < 1 {
		page = 1
	}
	pageSize := args.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	result, err := s.feeds.GetFeeds(ctx, args.Query, page, pageSize, feed.ParseSortOrder(args.Order))
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to list feeds")
	}
	return feedResult(feed.NewFeedsPage(result))
}

func (s *Service) handleGetFeed(ctx context.Context, _ mcp.CallToolRequest, args getFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.get_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	f, err := s.feeds.GetFeedByID(ctx, args.ID)
	if err != nil {
		return nil, mapSentinel(ctx, err, "Failed to get feed")
	}
	return feedResult(*f)
}

func (s *Service) handleCreateFeed(ctx context.Context, _ mcp.CallToolRequest, args createFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.create_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return nil, ErrNotFound
	}
	m := &feed.Feed{UserID: userID, Title: args.Title, Description: args.Description}
	if err := s.feeds.CreateFeed(ctx, m); err != nil {
		return nil, mapSentinel(ctx, err, "Failed to create feed")
	}
	return feedResult(*m)
}

func (s *Service) handleUpdateFeed(ctx context.Context, _ mcp.CallToolRequest, args updateFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.update_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return nil, ErrNotFound
	}
	m := &feed.Feed{UserID: userID, Title: args.Title, Description: args.Description}
	if err := s.feeds.UpdateFeed(ctx, args.ID, m); err != nil {
		return nil, mapSentinel(ctx, err, "Failed to update feed")
	}
	return feedResult(*m)
}

func (s *Service) handleDeleteFeed(ctx context.Context, _ mcp.CallToolRequest, args deleteFeedArgs) (*mcp.CallToolResult, error) {
	ctx, span := s.tracer.Start(ctx, "mcp.dispatch.delete_feed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	if err := s.feeds.DeleteFeed(ctx, args.ID); err != nil {
		return nil, mapSentinel(ctx, err, "Failed to delete feed")
	}
	// Lightweight confirmation: agents don't need a full Feed back,
	// just a positive signal the call succeeded.
	raw, _ := json.Marshal(map[string]any{"deleted": true, "id": args.ID})
	return &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.NewTextContent(string(raw))},
		StructuredContent: map[string]any{"deleted": true, "id": args.ID},
	}, nil
}

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

// mapSentinel translates feed.* sentinels into the MCP-layer sentinels
// defined above. safeDetail is the static literal that api.ClassifyAndLog
// writes to the wire envelope on the unknown-error path — never
// err.Error(). Caller must propagate the returned error; mcp-go renders
// it as a CallToolResult with IsError=true when returned by a tool
// handler.
func mapSentinel(ctx context.Context, err error, safeDetail string) error {
	if errors.Is(err, feed.ErrNotFound) {
		return ErrNotFound
	}
	api.ClassifyAndLog(ctx, err, safeDetail)
	return ErrInternal
}
