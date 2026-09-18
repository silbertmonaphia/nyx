package feed

import (
	"time"
)

// Feed is the on-wire and persistence shape for a feed record — used
// as the response body for GET/POST/PUT and as the sqlc projection.
// Server-generated fields (ID, CreatedAt, UpdatedAt, DeletedAt) are
// non-pointer value types and would trip huma's "required" rule if
// reused as a request body. Use FeedInput on the create/update
// operations instead; the repo fills in the server-generated fields
// from the DB row after the write.
//
// The `db` tags drive sqlc's column mapping; the `json` tags drive
// wire format. Huma validation tags are kept for documentation but
// only FeedInput enforces them on a real request path.
type Feed struct {
	ID          int        `json:"id" db:"id"`
	Title       string     `json:"title" db:"title" required:"true" minLength:"1" maxLength:"100"`
	Description string     `json:"description" db:"description" maxLength:"1000"`
	Rating      float64    `json:"rating" db:"rating" minimum:"0" maximum:"10"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}

// FeedInput is the user-supplied subset of fields accepted by POST and
// PUT /api/feeds. Server-generated fields are deliberately excluded
// so huma does not require the caller to send them — this matches the
// user-domain RegisterRequest pattern (separate request DTO from the
// on-wire/persistence shape). The handler converts FeedInput → Feed
// before calling the service; the repo then overwrites the Feed with
// the full DB row (including ID and timestamps).
type FeedInput struct {
	Title       string  `json:"title" required:"true" minLength:"1" maxLength:"100"`
	Description string  `json:"description" required:"false" maxLength:"1000"`
	Rating      float64 `json:"rating" required:"false" minimum:"0" maximum:"10"`
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