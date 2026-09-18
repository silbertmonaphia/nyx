package feed

import (
	"time"
)

// Feed is the on-wire and persistence shape for a feed record. The
// huma validation tags drive the request validation performed by the
// create / update operations; the `db` tags drive sqlc's column
// mapping; the `json` tags drive wire format (huma respects these for
// both input and output bodies).
type Feed struct {
	ID          int        `json:"id" db:"id"`
	Title       string     `json:"title" db:"title" required:"true" minLength:"1" maxLength:"100"`
	Description string     `json:"description" db:"description" maxLength:"1000"`
	Rating      float64    `json:"rating" db:"rating" minimum:"0" maximum:"10"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}

// FeedsPage is the paginated response envelope returned by GET /feeds.
type FeedsPage struct {
	Data     []Feed  `json:"data"`
	Page     int     `json:"page" example:"1"`
	PageSize int     `json:"page_size" example:"20"`
	Total    int     `json:"total" example:"42"`
	HasMore  bool    `json:"has_more" example:"true"`
}

// NewFeedsPage converts a repository Page into the response envelope.
// Empty result sets must serialize as [] not null.
func NewFeedsPage(p *Page) FeedsPage {
	items := p.Items
	if items == nil {
		items = []Feed{}
	}
	return FeedsPage{
		Data:     items,
		Page:     p.Page,
		PageSize: p.PageSize,
		Total:    p.Total,
		HasMore:  p.Page*p.PageSize < p.Total,
	}
}