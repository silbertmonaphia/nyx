package movie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nyx/internal/middleware"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func setupTestRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.CORS())

	api := router.Group("/api")
	{
		api.GET("/health", h.HealthHandler)
		api.GET("/movies", h.GetMoviesHandler)
		api.POST("/movies", h.CreateMovieHandler)
		api.PUT("/movies/:id", h.UpdateMovieHandler)
		api.DELETE("/movies/:id", h.DeleteMovieHandler)
	}
	return router
}

func TestHealthHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	mock.ExpectPing()

	router := setupTestRouter(h)
	req, _ := http.NewRequest("GET", "/api/health", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var response map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &response)
	if response["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", response["status"])
	}
}

func TestHealthHandlerError(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	mock.ExpectPing().WillReturnError(fmt.Errorf("db connection failed"))

	router := setupTestRouter(h)
	req, _ := http.NewRequest("GET", "/api/health", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %v", rr.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &response)
	if response["status"] != "error" {
		t.Errorf("expected status 'error', got %v", response["status"])
	}
}

func TestGetMoviesHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(1, "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8, now, now, nil).
		AddRow(2, "The Matrix", "A computer hacker learns from mysterious rebels about the true nature of his reality.", 8.7, now, now, nil)

	mock.ExpectQuery("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL").WillReturnRows(rows)

	router := setupTestRouter(h)
	req, _ := http.NewRequest("GET", "/api/movies", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var movies []Movie
	json.Unmarshal(rr.Body.Bytes(), &movies)
	if len(movies) != 2 {
		t.Errorf("expected 2 movies, got %v", len(movies))
	}
}

func TestCreateMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	newMovie := Movie{
		Title:       "Interstellar",
		Description: "Space exploration",
		Rating:      8.6,
	}
	body, _ := json.Marshal(newMovie)

	now := time.Now()
	mock.ExpectQuery("INSERT INTO movies").
		WithArgs(newMovie.Title, newMovie.Description, newMovie.Rating).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(1, now, now))

	router := setupTestRouter(h)
	req, _ := http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusCreated)
	}

	var m Movie
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m.ID != 1 {
		t.Errorf("expected ID 1, got %v", m.ID)
	}
}

func TestCreateMovieHandlerValidation(t *testing.T) {
	h := NewHandler(nil) // service not needed for validation usually, but Gin will call it if validation passes
	router := setupTestRouter(h)

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
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	updatedMovie := Movie{
		Title:       "Inception Updated",
		Description: "A deeper dream.",
		Rating:      9.0,
	}
	body, _ := json.Marshal(updatedMovie)

	now := time.Now()
	mock.ExpectQuery("UPDATE movies SET title = \\$1, description = \\$2, rating = \\$3, updated_at = CURRENT_TIMESTAMP WHERE id = \\$4 AND deleted_at IS NULL RETURNING created_at, updated_at").
		WithArgs(updatedMovie.Title, updatedMovie.Description, updatedMovie.Rating, 1).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(now, now))

	router := setupTestRouter(h)
	req, _ := http.NewRequest("PUT", "/api/movies/1", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
	}

	var m Movie
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m.ID != 1 || m.Title != updatedMovie.Title {
		t.Errorf("expected ID 1 and title %v, got ID %v and title %v", updatedMovie.Title, m.ID, m.Title)
	}
}

func TestDeleteMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	repo := NewRepository(mockDB)
	service := NewService(repo)
	h := NewHandler(service)

	mock.ExpectExec("UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs(1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	router := setupTestRouter(h)
	req, _ := http.NewRequest("DELETE", "/api/movies/1", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusNoContent)
	}
}
