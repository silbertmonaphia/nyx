package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestHealthHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectPing()

	req, err := http.NewRequest("GET", "/api/health", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(healthHandler)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	var response map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &response)
	if err != nil {
		t.Fatalf("could not unmarshal response: %v", err)
	}

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

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectPing().WillReturnError(fmt.Errorf("db connection failed"))

	req, _ := http.NewRequest("GET", "/api/health", nil)
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(healthHandler)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %v", status)
	}

	var response map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &response)
	if response["status"] != "error" {
		t.Errorf("expected status 'error', got %v", response["status"])
	}
}

func TestMiddleware(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := chain(mux, corsMiddleware, recoveryMiddleware)

	// Test CORS
	req, _ := http.NewRequest("OPTIONS", "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %v", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("expected CORS header, got %v", rr.Header().Get("Access-Control-Allow-Origin"))
	}

	// Test Recovery
	mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	req, _ = http.NewRequest("GET", "/panic", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for panic, got %v", rr.Code)
	}
}

func TestMoviesHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	// Replace global db with mock
	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(1, "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8, now, now, nil).
		AddRow(2, "The Matrix", "A computer hacker learns from mysterious rebels about the true nature of his reality.", 8.7, now, now, nil)

	mock.ExpectQuery("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL").WillReturnRows(rows)

	req, err := http.NewRequest("GET", "/api/movies", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	// Use the multiplexed handler from main to test CORS
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		moviesHandler(w, r)
	})

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	var movies []Movie
	err = json.Unmarshal(rr.Body.Bytes(), &movies)
	if err != nil {
		t.Errorf("could not unmarshal response: %v", err)
	}

	if len(movies) != 2 {
		t.Errorf("expected 2 movies, got %v", len(movies))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestMoviesHandlerError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectQuery("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL").WillReturnError(fmt.Errorf("db error"))

	req, _ := http.NewRequest("GET", "/api/movies", nil)
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(moviesHandler)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %v", status)
	}
}

func TestMoviesHandlerSearch(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	// Replace global db with mock
	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "title", "description", "rating", "created_at", "updated_at", "deleted_at"}).
		AddRow(1, "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8, now, now, nil)

	mock.ExpectQuery("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE title ILIKE \\$1 AND deleted_at IS NULL").
		WithArgs("%Incep%").
		WillReturnRows(rows)

	req, err := http.NewRequest("GET", "/api/movies?q=Incep", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(moviesHandler)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	var movies []Movie
	err = json.Unmarshal(rr.Body.Bytes(), &movies)
	if err != nil {
		t.Errorf("could not unmarshal response: %v", err)
	}

	if len(movies) != 1 {
		t.Errorf("expected 1 movie, got %v", len(movies))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestMoviesHandlerSearchError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectQuery("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE title ILIKE \\$1 AND deleted_at IS NULL").
		WithArgs("%Error%").
		WillReturnError(fmt.Errorf("db error"))

	req, _ := http.NewRequest("GET", "/api/movies?q=Error", nil)
	rr := httptest.NewRecorder()
	moviesHandler(rr, req)

	if status := rr.Code; status != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %v", status)
	}
}

func TestCreateMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

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

	req, err := http.NewRequest("POST", "/api/movies", bytes.NewBuffer(body))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(createMovieHandler)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusCreated {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusCreated)
	}

	var m Movie
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m.ID != 1 {
		t.Errorf("expected ID 1, got %v", m.ID)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestCreateMovieHandlerErrors(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	// Case 1: Invalid JSON
	req, _ := http.NewRequest("POST", "/api/movies", bytes.NewBufferString("{invalid-json}"))
	rr := httptest.NewRecorder()
	createMovieHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid JSON, got %v", rr.Code)
	}

	// Case 2: Empty Title
	req, _ = http.NewRequest("POST", "/api/movies", bytes.NewBufferString(`{"title":"","description":"test"}`))
	rr = httptest.NewRecorder()
	createMovieHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for empty title, got %v", rr.Code)
	}

	// Case 3: DB Error
	mock.ExpectQuery("INSERT INTO movies").WillReturnError(fmt.Errorf("insert error"))
	req, _ = http.NewRequest("POST", "/api/movies", bytes.NewBufferString(`{"title":"Error","description":"test"}`))
	rr = httptest.NewRecorder()
	createMovieHandler(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for DB error, got %v", rr.Code)
	}
}

func TestUpdateMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

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

	req, err := http.NewRequest("PUT", "/api/movies/1", bytes.NewBuffer(body))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(moviesRouter)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	var m Movie
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m.ID != 1 || m.Title != updatedMovie.Title {
		t.Errorf("expected ID 1 and title %v, got ID %v and title %v", updatedMovie.Title, m.ID, m.Title)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestUpdateMovieHandlerErrors(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	// Case 1: Invalid JSON
	req, _ := http.NewRequest("PUT", "/api/movies/1", bytes.NewBufferString("{invalid-json}"))
	rr := httptest.NewRecorder()
	updateMovieHandler(rr, req, 1)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid JSON, got %v", rr.Code)
	}

	// Case 2: Empty Title
	req, _ = http.NewRequest("PUT", "/api/movies/1", bytes.NewBufferString(`{"title":"","description":"test"}`))
	rr = httptest.NewRecorder()
	updateMovieHandler(rr, req, 1)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for empty title, got %v", rr.Code)
	}

	// Case 3: DB Error
	mock.ExpectQuery("UPDATE movies").WillReturnError(fmt.Errorf("update error"))
	req, _ = http.NewRequest("PUT", "/api/movies/1", bytes.NewBufferString(`{"title":"Error","description":"test"}`))
	rr = httptest.NewRecorder()
	updateMovieHandler(rr, req, 1)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for DB error, got %v", rr.Code)
	}

	// Case 4: Movie Not Found
	mock.ExpectQuery("UPDATE movies").WillReturnError(sql.ErrNoRows)
	req, _ = http.NewRequest("PUT", "/api/movies/999", bytes.NewBufferString(`{"title":"Not Found","description":"test"}`))
	rr = httptest.NewRecorder()
	updateMovieHandler(rr, req, 999)
	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status 404 for not found, got %v", rr.Code)
	}
}

func TestDeleteMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectExec("UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs(1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req, err := http.NewRequest("DELETE", "/api/movies/1", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(moviesRouter)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusNoContent {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusNoContent)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestDeleteMovieHandlerErrors(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error opening stub db: %s", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	// Case 1: DB Error
	mock.ExpectExec("UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = \\$1 AND deleted_at IS NULL").WillReturnError(fmt.Errorf("delete error"))
	req, _ := http.NewRequest("DELETE", "/api/movies/1", nil)
	rr := httptest.NewRecorder()
	deleteMovieHandler(rr, req, 1)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for DB error, got %v", rr.Code)
	}

	// Case 2: Movie Not Found
	mock.ExpectExec("UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = \\$1 AND deleted_at IS NULL").WillReturnResult(sqlmock.NewResult(0, 0))
	req, _ = http.NewRequest("DELETE", "/api/movies/999", nil)
	rr = httptest.NewRecorder()
	deleteMovieHandler(rr, req, 999)
	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status 404 for not found, got %v", rr.Code)
	}
}

func TestMoviesRouterErrors(t *testing.T) {
	// Case 1: Invalid ID format
	req, _ := http.NewRequest("GET", "/api/movies/abc", nil)
	rr := httptest.NewRecorder()
	moviesRouter(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid ID, got %v", rr.Code)
	}

	// Case 2: Unsupported method on base path
	req, _ = http.NewRequest("PATCH", "/api/movies", nil)
	rr = httptest.NewRecorder()
	moviesRouter(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405 for unsupported method, got %v", rr.Code)
	}

	// Case 3: Unsupported method on ID path
	req, _ = http.NewRequest("PATCH", "/api/movies/1", nil)
	rr = httptest.NewRecorder()
	moviesRouter(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405 for unsupported method on ID path, got %v", rr.Code)
	}
}
