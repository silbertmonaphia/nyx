package movie

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/cache"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v3"
	"go.opentelemetry.io/otel/trace/noop"
)

// setupTestRouter builds a chi + huma router wired with the same movie
// operations as the real API. We deliberately skip Prometheus, RequestID,
// Logging, CORS, and RateLimit — those have their own tests and add
// noise (and one goroutine in the case of RateLimit) to every handler
// test. The error envelope override is called once at the package level
// via TestMain in handler_test.go so validation errors also come back
// in the legacy shape.
//
// StoreRequest is installed so the auth middleware can recover the
// live *http.Request via reqctx.RequestFromContext — without it the
// cookie read would return ("", false) and every protected operation
// would 401. Production main.go installs StoreRequest early in the
// middleware chain for the same reason.
//
// The `withAuth` flag toggles whether the protected operations (POST/PUT/
// DELETE) carry the JWT middleware. Tests that don't exercise auth pass
// false; tests that want a 401 pass false and skip the cookie; tests
// that want a 200 pass true and mint a token via testAccessCookie.
// tokens is the per-test TokenService used to validate tokens minted
// by testAccessCookie.
//
// testCookieConfig mirrors what cmd/api/main.go passes in production;
// values don't have to match (tests don't assert on cookie attributes)
// but the CookieConfig is part of RegisterMovieOpsTest's signature.
func setupTestRouter(h *Handler, tokens auth.TokenService, withAuth bool) *chi.Mux {
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info:    &huma.Info{Title: "Nyx test", Version: "0.0.0"},
		},
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	})
	RegisterMovieOpsTest(hapi, h, tokens, testCookieConfig, withAuth)
	return router
}

// testCookieConfig is the CookieConfig passed into RegisterMovieOpsTest.
// Same shape as the user package's testCookieConfig — values don't
// matter for movie tests (the movie domain doesn't set cookies itself)
// but the struct is required to satisfy the signature.
var testCookieConfig = auth.CookieConfig{
	Secure:        false,
	Domain:        "",
	AccessName:    "__Host-nyx-access",
	RefreshName:   "__Host-nyx-refresh",
	AccessMaxAge:  15 * time.Minute,
	RefreshMaxAge: 7 * 24 * time.Hour,
	SameSite:      1, // http.SameSiteLaxMode
}

// testAccessCookie mints a fresh JWT and returns it as an
// http.Cookie ready for req.AddCookie. After L1 the cookie is the
// only auth source the middleware reads — Authorization: Bearer is
// intentionally ignored.
func testAccessCookie(t *testing.T, tokens auth.TokenService) *http.Cookie {
	t.Helper()
	tok, err := tokens.GenerateToken(1, "tester")
	if err != nil {
		t.Fatalf("mint test JWT: %v", err)
	}
	return &http.Cookie{Name: "nyx-access", Value: tok}
}

// newMockRepo wires a pgxmock pool through to NewRepository. The
// returned *pgxmock.PgxPoolIface is used by callers to set
// expectations; the constructor hands it to the repository which
// calls BeginTx, Exec, Query, QueryRow, and Ping through the same
// mock.
func newMockRepo(t *testing.T) (Repository, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { mock.Close() })
	return NewRepository(mock), mock
}

// newTestTokens builds a per-test TokenService. The service holds its
// own copy of the secret, so nothing process-wide is mutated. The TTL
// is irrelevant to handler tests — they only need a valid JWT.
func newTestTokens(t *testing.T) auth.TokenService {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	return tokens
}

// api.OverrideHumaErrors() is installed once via TestMain in
// repository_integration_test.go (Go allows only one TestMain per
// package; that file already owns the test bootstrap).

func TestHealthHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectPing()

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/health", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", response["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestHealthHandlerError(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectPing().WillReturnError(fmt.Errorf("db connection failed"))

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/health", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %v", rr.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response["status"] != "error" {
		t.Errorf("expected status 'error', got %v", response["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestGetMoviesHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	rows := pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(int32(1), "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8, now, now, nil).
		AddRow(int32(2), "The Matrix", "A computer hacker learns from mysterious rebels about the true nature of his reality.", 8.7, now, now, nil)

	// GetAll opens a tx, runs QueryMoviesPage + CountMovies, commits.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies`).
		WithArgs(pgxmock.AnyArg(), int32(0), int32(20)). // query=NULL, offset=0, page_size=20
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM movies`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(25)))
	mock.ExpectCommit()

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/movies", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page MoviesPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if len(page.Data) != 2 {
		t.Errorf("expected 2 movies, got %v", len(page.Data))
	}
	if page.Page != 1 || page.PageSize != 20 || page.Total != 25 {
		t.Errorf("unexpected page meta: page=%d page_size=%d total=%d", page.Page, page.PageSize, page.Total)
	}
	if !page.HasMore {
		t.Errorf("expected has_more=true when total > page*page_size")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestGetMoviesHandlerPaginationParams(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	rows := pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies`).
		WithArgs(pgxmock.AnyArg(), int32(10), int32(5)). // page=3, page_size=5 => offset=10
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM movies`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectCommit()

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/movies?page=3&page_size=5", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page MoviesPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if page.Page != 3 || page.PageSize != 5 {
		t.Errorf("expected page=3 page_size=5, got page=%d page_size=%d", page.Page, page.PageSize)
	}
	if page.HasMore {
		t.Errorf("expected has_more=false when total=0")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestGetMoviesHandlerPageSizeClamped(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	rows := pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies`).
		WithArgs(pgxmock.AnyArg(), int32(0), int32(100)). // page_size=500 clamps to 100
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM movies`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectCommit()

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/movies?page_size=500", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page MoviesPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if page.PageSize != 100 {
		t.Errorf("expected page_size clamped to 100, got %d", page.PageSize)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestGetMoviesHandlerSearch(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	rows := pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(int32(3), "The Matrix Reloaded", "Continuation of the Matrix saga.", 7.2, now, now, nil)

	// Search uses the SAME query as the no-search case (sqlc.narg),
	// but the query arg is now a non-NULL pgtype.Text with the
	// pattern. pgxmock matches args by driver value, but the
	// exact type wrapping (pgtype.Text vs raw string) depends on
	// pgx's encoder — AnyArg keeps the test focused on the SQL
	// contract and the offset/page_size order.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies`).
		WithArgs(pgxmock.AnyArg(), int32(0), int32(20)).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM movies`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectCommit()

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/movies?q=matrix", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page MoviesPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Title != "The Matrix Reloaded" {
		t.Errorf("expected one Matrix movie, got %+v", page.Data)
	}
	if page.Total != 1 {
		t.Errorf("expected total=1, got %d", page.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestCreateMovieHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	newMovie := Movie{
		Title:       "Interstellar",
		Description: "Space exploration",
		Rating:      8.6,
	}
	body, _ := json.Marshal(newMovie)

	now := time.Now()
	// sqlc-generated INSERT uses RETURNING * (all 7 columns) and pgx
	// encodes nullable fields as pgtype.Text / pgtype.Float8. We
	// pin the title (non-nullable) and match the others with AnyArg
	// to keep the test focused on the SQL contract, not on pgx's
	// internal value encoding.
	mock.ExpectQuery(`INSERT INTO movies`).
		WithArgs(newMovie.Title, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
			AddRow(int32(1), newMovie.Title, newMovie.Description, newMovie.Rating, now, now, nil))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusCreated)
	}

	var m Movie
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ID != 1 {
		t.Errorf("expected ID 1, got %v", m.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestCreateMovieHandlerValidation(t *testing.T) {
	h := NewHandler(nil) // service not needed; validation rejects before the repo is called
	router := setupTestRouter(h, newTestTokens(t), false)

	// Case 1: Empty Title (Required)
	body, _ := json.Marshal(map[string]interface{}{
		"title":  "",
		"rating": 5.0,
	})
	req, _ := http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for empty title, got %v", rr.Code)
	}

	// Case 2: Rating out of range
	body, _ = json.Marshal(map[string]interface{}{
		"title":  "Test",
		"rating": 11.0,
	})
	req, _ = http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid rating, got %v", rr.Code)
	}
}

func TestUpdateMovieHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	updatedMovie := Movie{
		Title:       "Inception Updated",
		Description: "A deeper dream.",
		Rating:      9.0,
	}
	body, _ := json.Marshal(updatedMovie)

	now := time.Now()
	// sqlc-generated UPDATE uses RETURNING * (all 7 columns).
	// pgx encodes the description and rating as pgtype.Text /
	// pgtype.Float8 wrappers; matching by type in the test is
	// brittle (and orthogonal to what we're testing), so we use
	// AnyArg() for the nullable fields and pin the int32 ID.
	mock.ExpectQuery(`UPDATE movies SET title`).
		WithArgs(updatedMovie.Title, pgxmock.AnyArg(), pgxmock.AnyArg(), int32(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
			AddRow(int32(1), updatedMovie.Title, updatedMovie.Description, updatedMovie.Rating, now, now, nil))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/movies/1", bytes.NewBuffer(body))
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var m Movie
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ID != 1 || m.Title != updatedMovie.Title {
		t.Errorf("expected ID 1 and title %v, got ID %v and title %v", updatedMovie.Title, m.ID, m.Title)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestDeleteMovieHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE movies SET deleted_at`).
		WithArgs(int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/movies/1", nil)
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusNoContent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestUpdateMovieHandlerNotFound exercises the new errors.Is(err,
// ErrNotFound) path. The repo returns pgx.ErrNoRows from the
// sqlc-generated :one query when the row is missing, and the
// handler maps that to HTTP 404.
func TestUpdateMovieHandlerNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	updatedMovie := Movie{Title: "Anything", Rating: 5.0}
	body, _ := json.Marshal(updatedMovie)

	mock.ExpectQuery(`UPDATE movies SET title`).
		WithArgs(updatedMovie.Title, pgxmock.AnyArg(), pgxmock.AnyArg(), int32(999)).
		WillReturnError(pgx.ErrNoRows)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/movies/999", bytes.NewBuffer(body))
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for missing movie, got %v", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestDeleteMovieHandlerNotFound exercises the new RowsAffected==0
// path in the repo. The handler maps ErrNotFound to HTTP 404.
func TestDeleteMovieHandlerNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE movies SET deleted_at`).
		WithArgs(int32(999)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/movies/999", nil)
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for missing movie, got %v", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// Sanity: ensure ErrNotFound is the right sentinel.
func TestErrNotFoundIsError(t *testing.T) {
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Error("ErrNotFound should be comparable via errors.Is")
	}
}

// TestCreateMovieHandler_InternalErrorHidesInternalDetails pins the
// safe-detail guarantee of commit 4: when the repo returns a non-
// sentinel error, the response body must carry a static wire message
// (now "Internal server error" via MapError) and must NOT echo the
// underlying pgx error.
func TestCreateMovieHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	body, _ := json.Marshal(Movie{Title: "X", Rating: 5})
	// pgx-style error text — contains SQL fragment & driver internals
	// we explicitly must not leak to the client.
	pgxLeak := fmt.Errorf("ERROR: relation %q does not exist (SQLSTATE 42P01)", "movies")
	mock.ExpectQuery(`INSERT INTO movies`).
		WithArgs("X", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgxLeak)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}

	var env api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Internal server error" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Internal server error")
	}
	if env.Details != "Failed to create movie" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to create movie")
	}
	if strings.Contains(rr.Body.String(), "42P01") || strings.Contains(rr.Body.String(), "SQLSTATE") {
		t.Errorf("response leaks pgx internals: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestUpdateMovieHandler_InternalErrorHidesInternalDetails covers the
// non-sentinel update branch — same wire contract as create, funneled
// through MapError.
func TestUpdateMovieHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	body, _ := json.Marshal(Movie{Title: "X", Rating: 5})
	mock.ExpectQuery(`UPDATE movies SET title`).
		WithArgs("X", pgxmock.AnyArg(), pgxmock.AnyArg(), int32(1)).
		WillReturnError(errors.New("pq: SSL connection has been closed unexpectedly"))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/movies/1", bytes.NewBuffer(body))
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}

	var env api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Details != "Failed to update movie" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to update movie")
	}
	if strings.Contains(rr.Body.String(), "SSL connection") {
		t.Errorf("response leaks pgx/SSL error text: %s", rr.Body.String())
	}
}

// TestDeleteMovieHandler_InternalErrorHidesInternalDetails covers the
// non-sentinel delete branch, funneled through MapError.
func TestDeleteMovieHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE movies SET deleted_at`).
		WithArgs(int32(1)).
		WillReturnError(errors.New("bcrypt: secret mismatch"))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/movies/1", nil)
	req.AddCookie(testAccessCookie(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}

	var env api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Details != "Failed to delete movie" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to delete movie")
	}
	if strings.Contains(rr.Body.String(), "bcrypt") {
		t.Errorf("response leaks library internals: %s", rr.Body.String())
	}
}
