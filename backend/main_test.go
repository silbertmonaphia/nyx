package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestHealthHandler(t *testing.T) {
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

	expected := `{"status": "ok", "message": "Douban Lite API is running"}`
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
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

	rows := sqlmock.NewRows([]string{"id", "title", "description", "rating"}).
		AddRow(1, "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8).
		AddRow(2, "The Matrix", "A computer hacker learns from mysterious rebels about the true nature of his reality.", 8.7)

	mock.ExpectQuery("SELECT id, title, description, rating FROM movies").WillReturnRows(rows)

	req, err := http.NewRequest("GET", "/api/movies", nil)
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

	if len(movies) != 2 {
		t.Errorf("expected 2 movies, got %v", len(movies))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
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

	rows := sqlmock.NewRows([]string{"id", "title", "description", "rating"}).
		AddRow(1, "Inception", "A thief who steals corporate secrets through the use of dream-sharing technology.", 8.8)

	mock.ExpectQuery("SELECT id, title, description, rating FROM movies WHERE title ILIKE \\$1").
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

	if movies[0].Title != "Inception" {
		t.Errorf("expected movie Inception, got %v", movies[0].Title)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}
