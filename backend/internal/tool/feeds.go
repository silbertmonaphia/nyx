package tool

import (
	"context"

	"nyx/internal/feed"
	"nyx/internal/reqctx"
)

// Tool name constants for the feed domain tools. These names are
// part of the MCP wire contract (`tools/list` + `tools/call`) and
// must stay byte-for-byte stable. The MCP package re-exports them
// as constants of its own so a typo at a call site fails to compile.
const (
	ToolNameListFeeds  = "list_feeds"
	ToolNameGetFeed    = "get_feed"
	ToolNameCreateFeed = "create_feed"
	ToolNameUpdateFeed = "update_feed"
	ToolNameDeleteFeed = "delete_feed"
)

// Arg types for the feed tools. Exported so the MCP shim (and any
// future caller) can construct populated values for testing.
//
// Struct tags drive the registry's input validator. The validate:"…"
// syntax mirrors the lightweight contract declared in
// internal/tool/validate.go — supported rules are required / min=N /
// max=N / oneof=… . The validator runs BEFORE the tool body fires,
// so a tag violation means the body never sees bad input.
//
// Defaults (page < 1 → 1, page_size < 1 → 20, page_size > 100 → 100,
// unknown order → desc) are NOT validation — they are normalisations
// applied inside the tool body so a hostile or sloppy caller still
// gets a well-formed request against feed.Service.

type ListFeedsArgs struct {
	// Query is bounded by length but empty strings are fine (no
	// filter). Order is normalised by feed.ParseSortOrder inside
	// the tool body — empty / unknown fall back to desc.
	//
	// Page / PageSize intentionally have no `min` constraint:
	// zero is "unspecified" and the tool body defaults to 1 / 20
	// (with page_size clamped to maxPageSize=100). The pre-
	// migration MCP handler accepted zero inputs the same way, so
	// the wire contract stays identical.
	Query    string `json:"query,omitempty" validate:"max=200"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
	Order    string `json:"order,omitempty"`
}

type GetFeedArgs struct {
	ID int `json:"id" validate:"required,min=1"`
}

type CreateFeedArgs struct {
	Title       string `json:"title"                   validate:"required,min=1,max=100"`
	Description string `json:"description,omitempty"   validate:"max=1000"`
}

type UpdateFeedArgs struct {
	ID          int    `json:"id"                      validate:"required,min=1"`
	Title       string `json:"title"                   validate:"required,min=1,max=100"`
	Description string `json:"description,omitempty"   validate:"max=1000"`
}

type DeleteFeedArgs struct {
	ID int `json:"id" validate:"required,min=1"`
}

// RegisterFeedTools populates reg with the five feed CRUD tools.
// feeds is the existing feed.Service — the tool bodies are thin
// shims that adapt typed arguments to the service's interface and
// surface feed.ErrNotFound unchanged so the caller's mapping code
// (errors.Is) keeps working.
//
// OTel: each tool opens a "tool.dispatch.<name>" span via the same
// tracer the feed service already uses, so the registry migration
// doesn't lose the existing trace surface.
func RegisterFeedTools(reg *Registry, feeds feed.Service) {
	reg.Register(Tool{
		Name:        ToolNameListFeeds,
		Version:     "1",
		Description: "List the authenticated caller's feeds. Supports search (max 200 chars), page (min 1, default 1), page_size (clamped to 100, default 20), and order (asc|desc, default desc). Returns the standard pagination envelope: {data, page, page_size, total, has_more}.",
		ArgType:     ListFeedsArgs{},
		Dispatch:    makeListFeedsDispatch(feeds),
	})

	reg.Register(Tool{
		Name:        ToolNameGetFeed,
		Version:     "1",
		Description: "Read a single feed by id. Returns an error result when the feed does not exist OR is owned by another user (single sentinel, no existence leak).",
		ArgType:     GetFeedArgs{},
		Dispatch:    makeGetFeedDispatch(feeds),
	})

	reg.Register(Tool{
		Name:        ToolNameCreateFeed,
		Version:     "1",
		Description: "Create a new feed. title is required (1-100 chars); description is optional (max 1000 chars). Returns the created feed (with server-generated id, created_at, updated_at).",
		ArgType:     CreateFeedArgs{},
		Dispatch:    makeCreateFeedDispatch(feeds),
	})

	reg.Register(Tool{
		Name:        ToolNameUpdateFeed,
		Version:     "1",
		Description: "Update an existing feed. id is required; title is required (1-100 chars); description is optional (max 1000 chars). Returns an error result if the feed does not exist OR is owned by another user.",
		ArgType:     UpdateFeedArgs{},
		Dispatch:    makeUpdateFeedDispatch(feeds),
	})

	reg.Register(Tool{
		Name:        ToolNameDeleteFeed,
		Version:     "1",
		Description: "Soft-delete a feed by id. Returns an error result if the feed does not exist OR is owned by another user.",
		ArgType:     DeleteFeedArgs{},
		Dispatch:    makeDeleteFeedDispatch(feeds),
	})
}

const maxPageSize = 100

func makeListFeedsDispatch(feeds feed.Service) func(context.Context, any) (any, error) {
	return func(ctx context.Context, raw any) (any, error) {
		args := raw.(ListFeedsArgs)
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
		result, err := feeds.GetFeeds(ctx, args.Query, page, pageSize, feed.ParseSortOrder(args.Order))
		if err != nil {
			return nil, err
		}
		return feed.NewFeedsPage(result), nil
	}
}

func makeGetFeedDispatch(feeds feed.Service) func(context.Context, any) (any, error) {
	return func(ctx context.Context, raw any) (any, error) {
		args := raw.(GetFeedArgs)
		return feeds.GetFeedByID(ctx, args.ID)
	}
}

func makeCreateFeedDispatch(feeds feed.Service) func(context.Context, any) (any, error) {
	return func(ctx context.Context, raw any) (any, error) {
		args := raw.(CreateFeedArgs)
		m := &feed.Feed{UserID: reqctx.UserIDFromContext(ctx), Title: args.Title, Description: args.Description}
		if err := feeds.CreateFeed(ctx, m); err != nil {
			return nil, err
		}
		return *m, nil
	}
}

func makeUpdateFeedDispatch(feeds feed.Service) func(context.Context, any) (any, error) {
	return func(ctx context.Context, raw any) (any, error) {
		args := raw.(UpdateFeedArgs)
		m := &feed.Feed{UserID: reqctx.UserIDFromContext(ctx), Title: args.Title, Description: args.Description}
		if err := feeds.UpdateFeed(ctx, args.ID, m); err != nil {
			return nil, err
		}
		return *m, nil
	}
}

func makeDeleteFeedDispatch(feeds feed.Service) func(context.Context, any) (any, error) {
	return func(ctx context.Context, raw any) (any, error) {
		args := raw.(DeleteFeedArgs)
		if err := feeds.DeleteFeed(ctx, args.ID); err != nil {
			return nil, err
		}
		return struct {
			Deleted bool `json:"deleted"`
			ID      int  `json:"id"`
		}{Deleted: true, ID: args.ID}, nil
	}
}

// Dispatcher bodies above call reqctx.UserIDFromContext directly.
// The registry guarantees the id is non-zero by the time the body
// runs (ErrMissingOwner short-circuits before the dispatch call).
