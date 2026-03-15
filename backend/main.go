package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"context"
	"os/signal"
	"syscall"
)

type Movie struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Rating      float64 `json:"rating"`
}

var db *sql.DB

func main() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(os.Stdout)

	var err error
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal().Msg("DB_URL environment variable is required")
	}

	// Retry loop for database connection
	for i := 0; i < 10; i++ {
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
			err = db.Ping()
			if err == nil {
				log.Info().Msg("Successfully connected to the database")
				break
			}
		}
		log.Warn().
			Int("attempt", i+1).
			Int("max_attempts", 10).
			Err(err).
			Msg("Could not connect to DB")
		time.Sleep(3 * time.Second)
	}

	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database after retries")
	}
	defer db.Close()

	// Run database migrations
	runMigrations(dbURL)

	// Create a cancellable context for handling signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Set up router and server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", healthHandler)
	mux.HandleFunc("/api/movies", moviesRouter)
	mux.HandleFunc("/api/movies/", moviesRouter)

	port := ":8080"
	server := &http.Server{
		Addr:    port,
		Handler: mux,
	}

	// Start server in a goroutine
	go func() {
		log.Info().Str("port", port).Msg("Server starting")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Server failed to start")
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()

	// Graceful shutdown
	log.Info().Msg("Shutting down server gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatal().Err(err).Msg("Server shutdown failed")
	}

	log.Info().Msg("Server exited properly")
}

func moviesRouter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Handle /api/movies and /api/movies/ as base path
	path := r.URL.Path
	idStr := strings.TrimPrefix(path, "/api/movies")
	idStr = strings.TrimPrefix(idStr, "/")

	if idStr == "" {
		if r.Method == http.MethodPost {
			createMovieHandler(w, r)
			return
		}
		if r.Method == http.MethodGet {
			moviesHandler(w, r)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Handle /api/movies/{id}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid movie ID", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodPut {
		updateMovieHandler(w, r, id)
		return
	}
	if r.Method == http.MethodDelete {
		deleteMovieHandler(w, r, id)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func runMigrations(dbURL string) {
	m, err := migrate.New(
		"file://migrations",
		dbURL,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create migration instance")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("An error occurred while running migrations")
	}

	log.Info().Msg("Database migrations applied successfully")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, `{"status": "ok", "message": "Nyx API is running"}`)
}

func moviesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	queryParam := r.URL.Query().Get("q")
	var rows *sql.Rows
	var err error

	if queryParam != "" {
		// Use ILIKE for case-insensitive search in Postgres
		rows, err = db.Query("SELECT id, title, description, rating FROM movies WHERE title ILIKE $1", "%"+queryParam+"%")
	} else {
		rows, err = db.Query("SELECT id, title, description, rating FROM movies")
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var movies []Movie
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.Title, &m.Description, &m.Rating); err != nil {
			log.Error().Err(err).Msg("Error scanning row")
			continue
		}
		movies = append(movies, m)
	}

	json.NewEncoder(w).Encode(movies)
}

func createMovieHandler(w http.ResponseWriter, r *http.Request) {
	var m Movie
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	if m.Title == "" {
		http.Error(w, "Title is required", http.StatusBadRequest)
		return
	}

	query := "INSERT INTO movies (title, description, rating) VALUES ($1, $2, $3) RETURNING id"
	err := db.QueryRow(query, m.Title, m.Description, m.Rating).Scan(&m.ID)
	if err != nil {
		log.Error().Err(err).Msg("Error inserting movie")
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(m)
}

func updateMovieHandler(w http.ResponseWriter, r *http.Request, id int) {
	var m Movie
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	if m.Title == "" {
		http.Error(w, "Title is required", http.StatusBadRequest)
		return
	}

	query := "UPDATE movies SET title = $1, description = $2, rating = $3 WHERE id = $4"
	res, err := db.Exec(query, m.Title, m.Description, m.Rating, id)
	if err != nil {
		log.Error().Err(err).Msg("Error updating movie")
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Movie not found", http.StatusNotFound)
		return
	}

	m.ID = id
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func deleteMovieHandler(w http.ResponseWriter, r *http.Request, id int) {
	query := "DELETE FROM movies WHERE id = $1"
	res, err := db.Exec(query, id)
	if err != nil {
		log.Error().Err(err).Msg("Error deleting movie")
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Movie not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
