package movie

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/rs/zerolog/log"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

// Handler exposes movie domain operations. It is constructed in main.go
// from a Service and registered onto a huma API via RegisterMovieOps.
type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RegisterMovieOps wires the movie endpoints onto a huma API. Each
// operation declares its inputs/outputs via huma struct tags so the
// generated OpenAPI 3.1 doc stays in sync with the wire contract. The
// mutating operations carry the Auth middleware in Operation.Middlewares
// — huma parses the body first, then runs Middlewares, then the
// handler. tokens supplies the JWT signing key to the per-operation
// middleware.
func RegisterMovieOps(api huma.API, h *Handler, tokens auth.TokenService) {
	RegisterMovieOpsTest(api, h, tokens, true)
}

// RegisterMovieOpsTest is the test-friendly variant of RegisterMovieOps.
// When `withAuth` is false, the protected operations are registered
// without the JWT middleware so tests can exercise the handler logic
// without minting tokens. Production code should always call
// RegisterMovieOps (which forces withAuth=true).
func RegisterMovieOpsTest(api huma.API, h *Handler, tokens auth.TokenService, withAuth bool) {
	huma.Register(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/api/health",
		Summary:     "Health check",
		Description: "Reports the status of the API, the database connection, and the cache.",
		Tags:        []string{"health"},
	}, h.Health)

	huma.Register(api, huma.Operation{
		OperationID: "get-movies",
		Method:      http.MethodGet,
		Path:        "/api/movies",
		Summary:     "List movies",
		Description: "Returns a paginated list of movies, optionally filtered by a search term matched against title and description.",
		Tags:        []string{"movies"},
	}, h.GetMovies)

	huma.Register(api, huma.Operation{
		OperationID: "create-movie",
		Method:      http.MethodPost,
		Path:        "/api/movies",
		Summary:     "Create a movie",
		Description: "Creates a new movie record. Requires a valid JWT in the Authorization header.",
		Tags:        []string{"movies"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.CreateMovie)

	huma.Register(api, huma.Operation{
		OperationID: "update-movie",
		Method:      http.MethodPut,
		Path:        "/api/movies/{id}",
		Summary:     "Update a movie",
		Description: "Updates the title, description, or rating of an existing movie. Requires a valid JWT.",
		Tags:        []string{"movies"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.UpdateMovie)

	huma.Register(api, huma.Operation{
		OperationID: "delete-movie",
		Method:      http.MethodDelete,
		Path:        "/api/movies/{id}",
		Summary:     "Delete a movie",
		Description: "Soft-deletes a movie record. Requires a valid JWT.",
		Tags:        []string{"movies"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: protectedMiddlewares(tokens, withAuth),
	}, h.DeleteMovie)
}

// protectedMiddlewares returns the per-operation middleware list for
// the protected movie operations. With withAuth=true it builds the JWT
// validator middleware from tokens; otherwise the list is empty so
// tests can exercise the handler without minting tokens.
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

type getMoviesInput struct {
	Q        string `query:"q" required:"false" doc:"Case-insensitive search term matched against title and description."`
	Page     int    `query:"page" required:"false" default:"1" minimum:"1" doc:"1-based page index."`
	PageSize int    `query:"page_size" required:"false" default:"20" minimum:"1" doc:"Items per page; clamped to a server-side maximum of 100."`
}

type getMoviesOutput struct{ Body MoviesPage }

type createMovieInput struct{ Body Movie }

type createMovieOutput struct {
	Status int `status:"201"`
	Body   Movie
}

type updateMovieInput struct {
	ID   int `path:"id" required:"true" minimum:"1"`
	Body Movie
}

type updateMovieOutput struct{ Body Movie }

type deleteMovieInput struct {
	ID int `path:"id" required:"true" minimum:"1"`
}

// deleteMovieOutput intentionally has no Body field — huma returns 204
// with an empty body when DefaultStatus = 204 and the Output is the
// zero value.

type deleteMovieOutput struct{}

// ---- Handler functions ----

func (h *Handler) Health(ctx context.Context, _ *struct{}) (*healthOutput, error) {
	dbStatus := "up"
	if err := h.service.CheckHealth(ctx); err != nil {
		dbStatus = "down"
		log.Error().Err(err).Msg("Database health check failed")
	}

	cacheStatus := "up"
	if err := h.service.CheckCacheHealth(ctx); err != nil {
		cacheStatus = "down"
		log.Error().Err(err).Msg("Cache health check failed")
	}

	status := "ok"
	if dbStatus == "down" {
		status = "error"
	}

	return &healthOutput{Body: healthResponse{
		Status: status,
		Services: healthServices{
			API:      "up",
			Database: dbStatus,
			Cache:    cacheStatus,
		},
	}}, nil
}

func (h *Handler) GetMovies(ctx context.Context, in *getMoviesInput) (*getMoviesOutput, error) {
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

	result, err := h.service.GetMovies(ctx, in.Q, page, pageSize)
	if err != nil {
		return nil, &api.ErrorResponse{
			Message: "Failed to retrieve movies",
			Code:    http.StatusInternalServerError,
			Details: api.ClassifyAndLog(ctx, err, "Failed to retrieve movies"),
		}
	}
	return &getMoviesOutput{Body: NewMoviesPage(result)}, nil
}

func (h *Handler) CreateMovie(ctx context.Context, in *createMovieInput) (*createMovieOutput, error) {
	if err := h.service.CreateMovie(ctx, &in.Body); err != nil {
		log.Error().Err(err).Msg("Error inserting movie")
		return nil, &api.ErrorResponse{
			Message: "Database error",
			Code:    http.StatusInternalServerError,
			Details: api.ClassifyAndLog(ctx, err, "Failed to create movie"),
		}
	}
	return &createMovieOutput{Status: http.StatusCreated, Body: in.Body}, nil
}

func (h *Handler) UpdateMovie(ctx context.Context, in *updateMovieInput) (*updateMovieOutput, error) {
	if err := h.service.UpdateMovie(ctx, in.ID, &in.Body); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, &api.ErrorResponse{Message: "Movie not found", Code: http.StatusNotFound}
		}
		log.Error().Err(err).Msg("Error updating movie")
		return nil, &api.ErrorResponse{
			Message: "Database error",
			Code:    http.StatusInternalServerError,
			Details: api.ClassifyAndLog(ctx, err, "Failed to update movie"),
		}
	}
	in.Body.ID = in.ID
	return &updateMovieOutput{Body: in.Body}, nil
}

func (h *Handler) DeleteMovie(ctx context.Context, in *deleteMovieInput) (*deleteMovieOutput, error) {
	if err := h.service.DeleteMovie(ctx, in.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, &api.ErrorResponse{Message: "Movie not found", Code: http.StatusNotFound}
		}
		log.Error().Err(err).Msg("Error deleting movie")
		return nil, &api.ErrorResponse{
			Message: "Database error",
			Code:    http.StatusInternalServerError,
			Details: api.ClassifyAndLog(ctx, err, "Failed to delete movie"),
		}
	}
	return &deleteMovieOutput{}, nil
}
