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

// AuthResponse is the JSON envelope returned by Login, Register, and
// Refresh. Tokens are NOT in the body — they ride in httpOnly
// __Host- cookies set by the handler (see auth.SetAuthCookies). The
// body carries only the user profile and the access-token expiry so
// the SPA can show "logged in as X" and decide when to pre-emptively
// refresh. The wire shape is intentionally narrower than the gin-era
// envelope; clients that previously read Token/RefreshToken must
// rely on cookie auto-attachment instead.
type AuthResponse struct {
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	User      User      `json:"user"`
}

// RefreshRequest / LogoutRequest now carry no body. The opaque
// refresh token rides in the __Host-nyx-refresh cookie; the handler
// reads it via auth.RefreshTokenFromCookie before invoking the
// service. huma's empty-body schema validates fine — the request
// type exists only to give huma something to bind against so the
// operation registers cleanly.
type RefreshRequest struct{}

type LogoutRequest struct{}
