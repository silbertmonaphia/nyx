package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

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
		dbURL = "postgres://postgres:postgres@localhost:5432/douban_lite?sslmode=disable"
	}

	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Initial ping to check connection
	err = db.Ping()
	if err != nil {
		log.Printf("Warning: Could not connect to DB yet: %v", err)
	}

	http.HandleFunc("/api/health", healthHandler)
	http.HandleFunc("/api/movies", moviesHandler)

	port := ":8080"
	log.Printf("Server starting on %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status": "ok", "message": "Douban Lite API is running"}`)
}

func moviesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Placeholder data for now until we have migrations
	movies := []Movie{
		{ID: 1, Title: "Inception", Description: "A thief who steals corporate secrets through the use of dream-sharing technology.", Rating: 8.8},
		{ID: 2, Title: "The Matrix", Description: "A computer hacker learns from mysterious rebels about the true nature of his reality.", Rating: 8.7},
	}

	json.NewEncoder(w).Encode(movies)
}
