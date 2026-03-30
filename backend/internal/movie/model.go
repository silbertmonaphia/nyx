package movie

import (
	"time"
)

type Movie struct {
	ID          int        `json:"id" db:"id"`
	Title       string     `json:"title" db:"title" binding:"required,min=1,max=100"`
	Description string     `json:"description" db:"description" binding:"max=1000"`
	Rating      float64    `json:"rating" db:"rating" binding:"min=0,max=10"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}
