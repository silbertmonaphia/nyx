package movie

import (
	"time"
)

type Movie struct {
	ID          int        `json:"id"`
	Title       string     `json:"title" binding:"required,min=1,max=100"`
	Description string     `json:"description" binding:"max=1000"`
	Rating      float64    `json:"rating" binding:"min=0,max=10"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}
