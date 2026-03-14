package main

import (
	"bytes"
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

	expected := `{"status": "ok", "message": "Nyx API is running"}`
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

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
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

	mock.ExpectQuery("INSERT INTO movies").
		WithArgs(newMovie.Title, newMovie.Description, newMovie.Rating).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))

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

	mock.ExpectExec("UPDATE movies SET title = \\$1, description = \\$2, rating = \\$3 WHERE id = \\$4").
		WithArgs(updatedMovie.Title, updatedMovie.Description, updatedMovie.Rating, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

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

func TestDeleteMovieHandler(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("an error '%s' was not expected when opening a stub database connection", err)
	}
	defer mockDB.Close()

	originalDB := db
	db = mockDB
	defer func() { db = originalDB }()

	mock.ExpectExec("DELETE FROM movies WHERE id = \\$1").
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
