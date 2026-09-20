package feed

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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v3"
	"go.opentelemetry.io/otel/trace/noop"
)

// setupTestRouter builds a chi + huma router wired with the same feed
// operations as the real API. We deliberately skip Prometheus, RequestID,
// Logging, CORS, and RateLimit — those have their own tests and add
// noise (and one goroutine in the case of RateLimit) to every handler
// test. The error envelope override is called once at the package level
// via TestMain in handler_test.go so validation errors also come back
// in the legacy shape.
//
// StoreRequest is installed so the auth middleware can recover the
// live *http.Request via reqctx.RequestFromContext — without it the
// Authorization header read would return ("", false) and every
// protected operation would 401. Production main.go installs
// StoreRequest early in the middleware chain for the same reason.
//
// The `withAuth` flag toggles whether the protected operations (POST/PUT/
// DELETE) carry the JWT middleware. Tests that don't exercise auth pass
// false; tests that want a 401 pass false and skip the token; tests
// that want a 200 pass true and mint a token via testAccessToken.
// tokens is the per-test TokenService used to validate tokens minted
// by testAccessToken.
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
	RegisterFeedOpsTest(hapi, h, tokens, withAuth)
	return router
}

// testAccessToken mints a fresh JWT for the default test user
// (id=1, "tester") and returns it as a string suitable for the
// Authorization: Bearer header. This is the only auth source the
// middleware reads.
func testAccessToken(t *testing.T, tokens auth.TokenService) string {
	t.Helper()
	return testAccessTokenFor(t, tokens, 1, "tester")
}

// testAccessTokenFor is the multi-user flavour of testAccessToken.
// Tests that exercise owner-scoping (cross-owner 404s) mint a second
// token via this helper.
func testAccessTokenFor(t *testing.T, tokens auth.TokenService, userID int, username string) string {
	t.Helper()
	tok, err := tokens.GenerateToken(userID, username)
	if err != nil {
		t.Fatalf("mint test JWT: %v", err)
	}
	return tok
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

// TestLivezHandlerReturnsOK — SECURITY.md M11. /api/livez is the
// SHALLOW liveness probe used by the backend container's Docker
// HEALTHCHECK. It must return 200 unconditionally as long as the
// process is serving HTTP — no DB ping, no Redis check, no
// dependency surface. The deep readiness signal lives on
// /api/health (TestHealthHandler / TestHealthHandlerError above).
//
// This test deliberately does NOT call mock.ExpectPing() — the
// contract is that the handler never touches the repo. If a future
// refactor adds a DB call here, pgxmock will fail the test with
// "unexpected query" and the regression is caught before it can
// turn a transient infra blip into a container restart cascade.
func TestLivezHandlerReturnsOK(t *testing.T) {
	repo, _ := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/livez", nil)
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
}

func TestGetFeedsHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(int32(1), int64(1), "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8, now, now, nil).
		AddRow(int32(2), int64(1), "The Matrix", "A computer hacker learns from mysterious rebels about the true nature of his reality.", 8.7, now, now, nil)

	// GetAll opens a tx, runs QueryFeedsPage + CountFeeds, commits.
	// Args: query, user_id, offset, page_size — user_id is the
	// authenticated caller (1 in this test).
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(0), int32(20)).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(25)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page FeedsPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if len(page.Data) != 2 {
		t.Errorf("expected 2 feeds, got %v", len(page.Data))
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

func TestGetFeedsHandlerPaginationParams(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(10), int32(5)). // page=3, page_size=5 => offset=10
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds?page=3&page_size=5", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page FeedsPage
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

func TestGetFeedsHandlerPageSizeClamped(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(0), int32(100)). // page_size=500 clamps to 100
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds?page_size=500", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page FeedsPage
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

func TestGetFeedsHandlerOrderAscUsesAscQuery(t *testing.T) {
	// ?order=asc must route to the ASC sqlc query. The mock
	// expectations target the ORDER BY ASC literal; if the handler
	// fell through to the DESC query (regression) the
	// ExpectationsWereMet check would fail.
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(int32(1), int64(1), "Oldest", "first", 5.0, now, now, nil)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(0), int32(20)).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds?order=asc", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}
	if len(rr.Body.Bytes()) == 0 {
		t.Fatalf("empty response body")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestGetFeedsHandlerOrderRejectsUnknownValue(t *testing.T) {
	// Huma's enum tag on the input struct rejects anything outside
	// {asc, desc} with a 400 before the handler runs — the DB is
	// never touched.
	repo, _ := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	router := setupTestRouter(h, newTestTokens(t), false)
	req, _ := http.NewRequest("GET", "/api/feeds?order=sideways", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown order, got %v", rr.Code)
	}
}

func TestGetFeedsHandlerSearch(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(int32(3), int64(1), "The Matrix Reloaded", "Continuation of the Matrix saga.", 7.2, now, now, nil)

	// Search uses the SAME query as the no-search case (sqlc.narg),
	// but the query arg is now a non-NULL pgtype.Text with the
	// pattern. pgxmock matches args by driver value, but the
	// exact type wrapping (pgtype.Text vs raw string) depends on
	// pgx's encoder — AnyArg keeps the test focused on the SQL
	// contract and the offset/page_size order.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(0), int32(20)).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds?q=matrix", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var page FeedsPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Title != "The Matrix Reloaded" {
		t.Errorf("expected one Matrix feed, got %+v", page.Data)
	}
	if page.Total != 1 {
		t.Errorf("expected total=1, got %d", page.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestCreateFeedHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	newFeed := FeedInput{
		Title:       "Interstellar",
		Description: "Space exploration",
		Rating:      8.6,
	}
	body, _ := json.Marshal(newFeed)

	now := time.Now()
	// sqlc-generated INSERT now takes (user_id, title, description,
	// rating). pgx encodes the nullable fields as pgtype.Text /
	// pgtype.Float8; we pin the title and user_id (the new arg) and
	// use AnyArg for the rest.
	mock.ExpectQuery(`INSERT INTO feeds`).
		WithArgs(int64(1), newFeed.Title, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at", "user_id"}).
			AddRow(int32(1), newFeed.Title, newFeed.Description, newFeed.Rating, now, now, nil, int64(1)))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusCreated)
	}

	var f Feed
	if err := json.Unmarshal(rr.Body.Bytes(), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ID != 1 {
		t.Errorf("expected ID 1, got %v", f.ID)
	}
	if f.UserID != 1 {
		t.Errorf("expected UserID 1 (JWT subject), got %d", f.UserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestCreateFeedHandlerValidation(t *testing.T) {
	h := NewHandler(nil) // service not needed; validation rejects before the repo is called
	router := setupTestRouter(h, newTestTokens(t), false)

	// Case 1: Empty Title (Required)
	body, _ := json.Marshal(map[string]interface{}{
		"title":  "",
		"rating": 5.0,
	})
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
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
	req, _ = http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid rating, got %v", rr.Code)
	}
}

// TestCreateFeedHandlerUserPayload pins the regression where huma
// validated the POST body against the response/persistence Feed
// struct. Because Feed has non-pointer ID/CreatedAt/UpdatedAt, a body
// carrying only the user-supplied {title, description, rating} was
// rejected with "expected required property id/created_at/updated_at
// to be present". The fix is FeedInput — a request DTO that excludes
// the server-generated fields (matches the user-domain
// RegisterRequest pattern).
func TestCreateFeedHandlerUserPayload(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO feeds`).
		WithArgs(int64(1), "Inception", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at", "user_id"}).
			AddRow(int32(7), "Inception", "Dream heist", 8.8, now, now, nil, int64(1)))

	body, _ := json.Marshal(map[string]interface{}{
		"title":       "Inception",
		"description": "Dream heist",
		"rating":      8.8,
	})
	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var f Feed
	if err := json.Unmarshal(rr.Body.Bytes(), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ID != 7 || f.Title != "Inception" || f.Description != "Dream heist" || f.Rating != 8.8 {
		t.Errorf("unexpected response: %+v", f)
	}
	if f.UserID != 1 {
		t.Errorf("expected UserID 1 (from JWT), got %d", f.UserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestCreateFeedHandlerMinimalPayload confirms description and rating
// are optional on the request — only Title is required. The handler
// should accept {title} alone and leave description="" / rating=0 on
// the response (DB defaults; not validated client-side).
func TestCreateFeedHandlerMinimalPayload(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO feeds`).
		WithArgs(int64(1), "Bare Minimum", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at", "user_id"}).
			AddRow(int32(8), "Bare Minimum", "", 0, now, now, nil, int64(1)))

	body, _ := json.Marshal(map[string]interface{}{"title": "Bare Minimum"})
	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestUpdateFeedHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	updatedFeed := FeedInput{
		Title:       "Inception Updated",
		Description: "A deeper dream.",
		Rating:      9.0,
	}
	body, _ := json.Marshal(updatedFeed)

	now := time.Now()
	// sqlc-generated UPDATE: args are title, description, rating,
	// id, user_id. We pin the int32 id and the int64 user_id (the
	// new arg); the rest is AnyArg.
	mock.ExpectQuery(`UPDATE feeds SET title`).
		WithArgs(updatedFeed.Title, pgxmock.AnyArg(), pgxmock.AnyArg(), int32(1), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at", "user_id"}).
			AddRow(int32(1), updatedFeed.Title, updatedFeed.Description, updatedFeed.Rating, now, now, nil, int64(1)))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/feeds/1", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var f Feed
	if err := json.Unmarshal(rr.Body.Bytes(), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ID != 1 || f.Title != updatedFeed.Title {
		t.Errorf("expected ID 1 and title %v, got ID %v and title %v", updatedFeed.Title, f.ID, f.Title)
	}
	if f.UserID != 1 {
		t.Errorf("expected UserID 1 (from JWT), got %d", f.UserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

func TestDeleteFeedHandler(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE feeds SET deleted_at`).
		WithArgs(int32(1), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/feeds/1", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusNoContent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestUpdateFeedHandlerNotFound exercises the new errors.Is(err,
// ErrNotFound) path. The repo returns pgx.ErrNoRows from the
// sqlc-generated :one query when the row is missing, and the
// handler maps that to HTTP 404.
func TestUpdateFeedHandlerNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	updatedFeed := FeedInput{Title: "Anything", Rating: 5.0}
	body, _ := json.Marshal(updatedFeed)

	mock.ExpectQuery(`UPDATE feeds SET title`).
		WithArgs(updatedFeed.Title, pgxmock.AnyArg(), pgxmock.AnyArg(), int32(999), int64(1)).
		WillReturnError(pgx.ErrNoRows)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/feeds/999", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for missing feed, got %v", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestDeleteFeedHandlerNotFound exercises the new RowsAffected==0
// path in the repo. The handler maps ErrNotFound to HTTP 404.
func TestDeleteFeedHandlerNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE feeds SET deleted_at`).
		WithArgs(int32(999), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/feeds/999", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for missing feed, got %v", rr.Code)
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

// TestCreateFeedHandler_InternalErrorHidesInternalDetails pins the
// safe-detail guarantee of commit 4: when the repo returns a non-
// sentinel error, the response body must carry a static wire message
// (now "Internal server error" via MapError) and must NOT echo the
// underlying pgx error.
func TestCreateFeedHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	body, _ := json.Marshal(FeedInput{Title: "X", Rating: 5})
	// pgx-style error text — contains SQL fragment & driver internals
	// we explicitly must not leak to the client.
	pgxLeak := fmt.Errorf("ERROR: relation %q does not exist (SQLSTATE 42P01)", "feeds")
	mock.ExpectQuery(`INSERT INTO feeds`).
		WithArgs(int64(1), "X", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgxLeak)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
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
	if env.Details != "Failed to create feed" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to create feed")
	}
	if strings.Contains(rr.Body.String(), "42P01") || strings.Contains(rr.Body.String(), "SQLSTATE") {
		t.Errorf("response leaks pgx internals: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestUpdateFeedHandler_InternalErrorHidesInternalDetails covers the
// non-sentinel update branch — same wire contract as create, funneled
// through MapError.
func TestUpdateFeedHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	body, _ := json.Marshal(FeedInput{Title: "X", Rating: 5})
	mock.ExpectQuery(`UPDATE feeds SET title`).
		WithArgs("X", pgxmock.AnyArg(), pgxmock.AnyArg(), int32(1), int64(1)).
		WillReturnError(errors.New("pq: SSL connection has been closed unexpectedly"))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/feeds/1", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}

	var env api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Details != "Failed to update feed" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to update feed")
	}
	if strings.Contains(rr.Body.String(), "SSL connection") {
		t.Errorf("response leaks pgx/SSL error text: %s", rr.Body.String())
	}
}

// TestDeleteFeedHandler_InternalErrorHidesInternalDetails covers the
// non-sentinel delete branch, funneled through MapError.
func TestDeleteFeedHandler_InternalErrorHidesInternalDetails(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE feeds SET deleted_at`).
		WithArgs(int32(1), int64(1)).
		WillReturnError(errors.New("bcrypt: secret mismatch"))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/feeds/1", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}

	var env api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Details != "Failed to delete feed" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to delete feed")
	}
	if strings.Contains(rr.Body.String(), "bcrypt") {
		t.Errorf("response leaks library internals: %s", rr.Body.String())
	}
}

// TestGetFeedsHandler_RequiresAuth pins the auth gate on GET /api/feeds.
// The route is now owner-scoped and authenticated — a missing
// Authorization header must 401. The repo is never touched.
func TestGetFeedsHandler_RequiresAuth(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds", nil)
	// Deliberately no Authorization header.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Authorization header, got %v", rr.Code)
	}
	if mock.ExpectationsWereMet() != nil {
		t.Errorf("repo must not be queried on 401")
	}
}

// TestUpdateFeedHandler_OtherUserReturnsNotFound covers the
// leak-free cross-owner path. A user-2 token attempts to mutate a
// feed owned by user 1; the SQL WHERE filters on user_id, so the
// repo returns pgx.ErrNoRows (0 rows updated). The handler must map
// that to 404, NOT 403 — single ErrNotFound sentinel, no existence
// leak.
func TestUpdateFeedHandler_OtherUserReturnsNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	updatedFeed := FeedInput{Title: "Hijack", Rating: 9.9}
	body, _ := json.Marshal(updatedFeed)

	// Args: title, description, rating, id (1), user_id (2 — the
	// attacker, not the owner). pgx.ErrNoRows because no row matches
	// the id+user_id combo.
	mock.ExpectQuery(`UPDATE feeds SET title`).
		WithArgs(updatedFeed.Title, pgxmock.AnyArg(), pgxmock.AnyArg(), int32(1), int64(2)).
		WillReturnError(pgx.ErrNoRows)

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("PUT", "/api/feeds/1", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessTokenFor(t, tokens, 2, "intruder"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for cross-owner PUT, got %v", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestDeleteFeedHandler_OtherUserReturnsNotFound is the soft-delete
// counterpart of TestUpdateFeedHandler_OtherUserReturnsNotFound.
// SoftDeleteFeed returns RowsAffected=0 because no row matches the
// user_id+id combo — that maps to ErrNotFound → 404.
func TestDeleteFeedHandler_OtherUserReturnsNotFound(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	mock.ExpectExec(`UPDATE feeds SET deleted_at`).
		WithArgs(int32(1), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("DELETE", "/api/feeds/1", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessTokenFor(t, tokens, 2, "intruder"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for cross-owner DELETE, got %v", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestCreateFeedHandler_StampsUserID pins that the INSERT receives
// the JWT subject as the user_id arg — never the (zero) value from
// the request body and never a forged id from the body either. The
// repo stamps m.UserID before persisting so a future SELECT returns
// the row with the correct owner.
func TestCreateFeedHandler_StampsUserID(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	now := time.Now()
	// Verify the INSERT args contain user_id=1 (from the JWT
	// subject, not from the request body). AnyArg matches anything
	// for the description/rating fields. We use the pgtype
	// wrappers explicitly for nullable fields because pgxmock +
	// pgtype scan can silently zero out downstream columns when the
	// raw driver value looks like a SQL NULL (e.g. float64(0)
	// decoded into pgtype.Float8).
	mock.ExpectQuery(`INSERT INTO feeds`).
		WithArgs(int64(1), "Owned", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at", "user_id"}).
			AddRow(int32(42), "Owned", pgtype.Text{String: "", Valid: false}, pgtype.Float8{Float64: 0, Valid: true}, pgtype.Timestamptz{Time: now, Valid: true}, pgtype.Timestamptz{Time: now, Valid: true}, pgtype.Timestamptz{}, int64(1)))

	body, _ := json.Marshal(FeedInput{Title: "Owned"})
	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("POST", "/api/feeds", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var f Feed
	if err := json.Unmarshal(rr.Body.Bytes(), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.UserID != 1 {
		t.Errorf("expected UserID=1 stamped from JWT, got %d", f.UserID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestGetFeedsHandler_FilteredByOwner is the per-user WHERE clause
// guard: pgxmock arg matching asserts that the user_id passed to
// QueryFeedsPage / CountFeeds is the JWT subject (1), so a future
// regression that hardcoded 0 (or skipped the filter) would fail
// the test.
func TestGetFeedsHandler_FilteredByOwner(t *testing.T) {
	repo, mock := newMockRepo(t)
	service := NewService(repo, cache.NewNoop(), time.Minute, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(service)

	rows := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, user_id, title, description, rating, created_at, updated_at, deleted_at FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1), int32(0), int32(20)).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM feeds`).
		WithArgs(pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectCommit()

	tokens := newTestTokens(t)
	router := setupTestRouter(h, tokens, true)
	req, _ := http.NewRequest("GET", "/api/feeds", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessToken(t, tokens))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}