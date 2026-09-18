package feed

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/rs/zerolog/log"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

// Handler exposes feed domain operations. It is constructed in main.go
// from a Service and registered onto a huma API via RegisterFeedOps.
type Handler struct {
	service Service
}

// Health-check status string constants. Extracted so the repeated
// literals don't trip goconst in golangci-lint and so the wire format
// is documented in one place.
const (
	statusUp   = "up"
	statusDown = "down"
)

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RegisterFeedOps wires the feed endpoints onto a huma API. Each
// operation declares its inputs/outputs via huma struct tags so the
// generated OpenAPI 3.1 doc stays in sync with the wire contract. The
// mutating operations carry the Auth middleware in Operation.Middlewares
// — huma parses the body first, then runs Middlewares, then the
// handler. tokens supplies the JWT signing key to the per-operation
// middleware. cookieConfig supplies the access-cookie name + Secure
// flag so the middleware can read the token from the httpOnly
// cookie first (with Authorization: Bearer as the deprecation-
// window fallback).
func RegisterFeedOps(api huma.API, h *Handler, tokens auth.TokenService) {
	RegisterFeedOpsTest(api, h, tokens, true)
}

// RegisterFeedOpsTest is the test-friendly variant of RegisterFeedOps.
// When `withAuth` is false, the protected operations are registered
// without the JWT middleware so tests can exercise the handler logic
// without minting tokens. Production code should always call
// RegisterFeedOps (which forces withAuth=true).
func RegisterFeedOpsTest(api huma.API, h *Handler, tokens auth.TokenService, withAuth bool) {
	huma.Register(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/api/health",
		Summary:     "Health check",
		Description: "Reports the status of the API, the database connection, and the cache.",
		Tags:        []string{"health"},
	}, h.Health)

	// /api/livez — shallow liveness probe. Returns 200 unconditionally
	// as long as the process is up and serving HTTP. Used by the
	// backend container's Docker HEALTHCHECK so a transient DB or
	// Redis blip never flips the container to unhealthy and triggers
	// a restart cascade. The deep readiness signal (DB + cache status)
	// remains on /api/health — wire it to compose/K8s readiness when
	// you need a dependency-aware check. See SECURITY.md M11.
	huma.Register(api, huma.Operation{
		OperationID: "livez",
		Method:      http.MethodGet,
		Path:        "/api/livez",
		Summary:     "Liveness probe",
		Description: "Shallow liveness check — returns 200 as long as the process is up. Does NOT probe the database or cache; use /api/health for a deep readiness signal.",
		Tags:        []string{"health"},
	}, func(_ context.Context, _ *struct{}) (*livezOutput, error) {
		return &livezOutput{Body: livezResponse{Status: "ok"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-feeds",
		Method:      http.MethodGet,
		Path:        "/api/feeds",
		Summary:     "List feeds",
		Description: "Returns a paginated list of feeds, optionally filtered by a search term matched against title and description.",
		Tags:        []string{"feeds"},
	}, h.GetFeeds)

	huma.Register(api, huma.Operation{
		OperationID: "create-feed",
		Method:      http.MethodPost,
		Path:        "/api/feeds",
		Summary:     "Create a feed",
		Description: "Creates a new feed record. Requires a valid JWT in the Authorization header.",
		Tags:        []string{"feeds"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.CreateFeed)

	huma.Register(api, huma.Operation{
		OperationID: "update-feed",
		Method:      http.MethodPut,
		Path:        "/api/feeds/{id}",
		Summary:     "Update a feed",
		Description: "Updates the title, description, or rating of an existing feed. Requires a valid JWT.",
		Tags:        []string{"feeds"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.UpdateFeed)

	huma.Register(api, huma.Operation{
		OperationID: "delete-feed",
		Method:      http.MethodDelete,
		Path:        "/api/feeds/{id}",
		Summary:     "Delete a feed",
		Description: "Soft-deletes a feed record. Requires a valid JWT.",
		Tags:        []string{"feeds"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.DeleteFeed)
}

// protectedMiddlewares returns the per-operation middleware list for
// the protected feed operations. With withAuth=true it builds the JWT
// validator middleware from tokens; otherwise the list is empty so
// tests can exercise the handler without minting tokens. The
// validator reads Authorization: Bearer (RFC 6750) — same contract
// as every other auth path in the API.
func protectedMiddlewares(tokens auth.TokenService, withAuth bool) huma.Middlewares {
	if !withAuth {
		return nil
	}
	return huma.Middlewares{middleware.NewHumaAuth(tokens)}
}

// ---- Operation input / output structs ----
//
// Huma populates these from query / path / header / body based on the
// struct tags. Field types drive parameter parsing and the generated
// OpenAPI schema; required:"true" triggers automatic 400s before the
// handler is called.

type healthOutput struct {
	Body healthResponse
}

type healthResponse struct {
	Status   string         `json:"status" example:"ok" doc:"Overall status: 'ok' when the database is reachable, 'error' otherwise."`
	Services healthServices `json:"services"`
}

type healthServices struct {
	API      string `json:"api" example:"up"`
	Database string `json:"database" example:"up"`
	Cache    string `json:"cache" example:"up"`
}

// livezOutput / livezResponse are the wire types for /api/livez.
// Kept deliberately separate from healthResponse so a future
// health-shape change (adding new dependency checks) cannot
// accidentally widen the liveness contract.
type livezOutput struct {
	Body livezResponse
}

type livezResponse struct {
	Status string `json:"status" example:"ok" doc:"Always 'ok' as long as the process is serving HTTP."`
}

type getFeedsInput struct {
	// maxLength caps the search term so a multi-MB query string can't
	// reach Postgres. Huma emits a 400 on inputs over the limit before
	// the handler runs (see SECURITY.md M6).
	Q        string `query:"q" required:"false" maxLength:"200" doc:"Case-insensitive search term matched against title and description."`
	Page     int    `query:"page" required:"false" default:"1" minimum:"1" doc:"1-based page index."`
	PageSize int    `query:"page_size" required:"false" default:"20" minimum:"1" doc:"Items per page; clamped to a server-side maximum of 100."`
}

type getFeedsOutput struct{ Body FeedsPage }

type createFeedInput struct{ Body Feed }

type createFeedOutput struct {
	Status int `status:"201"`
	Body   Feed
}

type updateFeedInput struct {
	ID   int `path:"id" required:"true" minimum:"1"`
	Body Feed
}

type updateFeedOutput struct{ Body Feed }

type deleteFeedInput struct {
	ID int `path:"id" required:"true" minimum:"1"`
}

// deleteFeedOutput intentionally has no Body field — huma returns 204
// with an empty body when DefaultStatus = 204 and the Output is the
// zero value.

type deleteFeedOutput struct{}

// ---- Handler functions ----

// Health returns the liveness/readiness state for the API, its DB
// pool, and its cache backend. dbStatus / cacheStatus are "up" unless
// their respective Check*Health call returns an error; status is "ok"
// only when both upstream checks pass.
//
//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Health(ctx context.Context, _ *struct{}) (*healthOutput, error) {
	dbStatus := statusUp
	if err := h.service.CheckHealth(ctx); err != nil {
		dbStatus = statusDown
		log.Error().Err(err).Msg("Database health check failed")
	}

	cacheStatus := statusUp
	if err := h.service.CheckCacheHealth(ctx); err != nil {
		cacheStatus = statusDown
		log.Error().Err(err).Msg("Cache health check failed")
	}

	status := "ok"
	if dbStatus == statusDown {
		status = "error"
	}

	return &healthOutput{Body: healthResponse{
		Status: status,
		Services: healthServices{
			API:      statusUp,
			Database: dbStatus,
			Cache:    cacheStatus,
		},
	}}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetFeeds(ctx context.Context, in *getFeedsInput) (*getFeedsOutput, error) {
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	result, err := h.service.GetFeeds(ctx, in.Q, page, pageSize)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to retrieve feeds")
	}
	return &getFeedsOutput{Body: NewFeedsPage(result)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) CreateFeed(ctx context.Context, in *createFeedInput) (*createFeedOutput, error) {
	if err := h.service.CreateFeed(ctx, &in.Body); err != nil {
		return nil, api.MapError(ctx, err, "Failed to create feed")
	}
	return &createFeedOutput{Status: http.StatusCreated, Body: in.Body}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) UpdateFeed(ctx context.Context, in *updateFeedInput) (*updateFeedOutput, error) {
	if err := h.service.UpdateFeed(ctx, in.ID, &in.Body); err != nil {
		return nil, api.MapError(ctx, err, "Failed to update feed")
	}
	in.Body.ID = in.ID
	return &updateFeedOutput{Body: in.Body}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) DeleteFeed(ctx context.Context, in *deleteFeedInput) (*deleteFeedOutput, error) {
	if err := h.service.DeleteFeed(ctx, in.ID); err != nil {
		return nil, api.MapError(ctx, err, "Failed to delete feed")
	}
	return &deleteFeedOutput{}, nil
}