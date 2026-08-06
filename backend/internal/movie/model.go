package movie

import (
	"time"
)

// Movie is the on-wire and persistence shape for a movie record. The
// huma validation tags drive the request validation performed by the
// create / update operations; the `db` tags drive sqlc's column
// mapping; the `json` tags drive wire format (huma respects these for
// both input and output bodies).
type Movie struct {
	ID          int        `json:"id" db:"id"`
	Title       string     `json:"title" db:"title" required:"true" minLength:"1" maxLength:"100"`
	Description string     `json:"description" db:"description" maxLength:"1000"`
	Rating      float64    `json:"rating" db:"rating" minimum:"0" maximum:"10"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}

// MoviesPage is the paginated response envelope returned by GET /movies.
type MoviesPage struct {
	Data     []Movie `json:"data"`
	Page     int     `json:"page" example:"1"`
	PageSize int     `json:"page_size" example:"20"`
	Total    int     `json:"total" example:"42"`
	HasMore  bool    `json:"has_more" example:"true"`
}

// NewMoviesPage converts a repository Page into the response envelope.
// Empty result sets must serialize as [] not null.
func NewMoviesPage(p *Page) MoviesPage {
	items := p.Items
	if items == nil {
		items = []Movie{}
	}
	return MoviesPage{
		Data:     items,
		Page:     p.Page,
		PageSize: p.PageSize,
		Total:    p.Total,
		HasMore:  p.Page*p.PageSize < p.Total,
	}
}
