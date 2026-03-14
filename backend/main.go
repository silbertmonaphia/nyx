package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type Movie struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Rating      float64 `json:"rating"`
}

var db *sql.DB

func main() {
	var err error
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal("DB_URL environment variable is required")
	}

	// Retry loop for database connection
	for i := 0; i < 10; i++ {
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
			err = db.Ping()
			if err == nil {
				log.Println("Successfully connected to the database")
				break
			}
		}
		log.Printf("Warning: Could not connect to DB (attempt %d/10): %v", i+1, err)
		time.Sleep(3 * time.Second)
	}

	if err != nil {
		log.Fatalf("Could not connect to database after retries: %v", err)
	}
	defer db.Close()

	// Simple migration
	initDB()

	http.HandleFunc("/api/health", healthHandler)
	http.HandleFunc("/api/movies", moviesRouter)
	http.HandleFunc("/api/movies/", moviesRouter)

	port := ":8080"
	log.Printf("Server starting on %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
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

func initDB() {
	query := `
	CREATE TABLE IF NOT EXISTS movies (
		id SERIAL PRIMARY KEY,
		title TEXT NOT NULL,
		description TEXT,
		rating DOUBLE PRECISION
	);`
	_, err := db.Exec(query)
	if err != nil {
		log.Printf("Error creating table: %v", err)
		return
	}

	// Seed data if empty
	var count int
	db.QueryRow("SELECT COUNT(*) FROM movies").Scan(&count)
	if count == 0 {
		_, err = db.Exec(`
			INSERT INTO movies (title, description, rating) VALUES 
			('Inception', 'A thief who steals corporate secrets through the use of dream-sharing technology.', 8.8),
			('The Matrix', 'A computer hacker learns from mysterious rebels about the true nature of his reality.', 8.7),
			('Interstellar', 'A team of explorers travel through a wormhole in space in an attempt to ensure humanity''s survival.', 8.6);
		`)
		if err != nil {
			log.Printf("Error seeding data: %v", err)
		}
	}
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
			log.Printf("Error scanning row: %v", err)
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
		log.Printf("Error inserting movie: %v", err)
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
		log.Printf("Error updating movie: %v", err)
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
		log.Printf("Error deleting movie: %v", err)
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
