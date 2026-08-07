package user

import "time"

type User struct {
	ID           int        `json:"id" db:"id"`
	Username     string     `json:"username" db:"username"`
	Email        string     `json:"email" db:"email"`
	PasswordHash string     `json:"-" db:"password_hash"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}

// RegisterRequest / LoginRequest carry huma validation tags (not the
// gin-era `binding:` tags — huma uses its own dialect; see HUMA.md).
// Without these, a malformed payload reaches the service and bcrypt,
// producing a 500 on inputs that should reject at the edge.
type RegisterRequest struct {
	Username string `json:"username" required:"true" minLength:"3" maxLength:"50"`
	Email    string `json:"email" required:"true" format:"email"`
	Password string `json:"password" required:"true" minLength:"6"`
}

type LoginRequest struct {
	Username string `json:"username" required:"true"`
	Password string `json:"password" required:"true"`
}

type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}
