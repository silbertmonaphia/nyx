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
//
// Password length is bounded above as well as below: bcrypt itself
// truncates at 72 bytes and bcrypt on a multi-KB input wastes CPU.
// 128 bytes leaves headroom for a future shift to Argon2id or
// scrypt without breaking clients (see SECURITY.md M7).
type RegisterRequest struct {
	Username string `json:"username" required:"true" minLength:"3" maxLength:"50"`
	Email    string `json:"email" required:"true" format:"email"`
	Password string `json:"password" required:"true" minLength:"6" maxLength:"128"`
}

type LoginRequest struct {
	Username string `json:"username" required:"true"`
	Password string `json:"password" required:"true" maxLength:"128"`
}

// AuthResponse is the JSON envelope returned by Login, Register, and
// Refresh. Tokens ride in the BODY (RFC 6750 Bearer transport) — not
// in cookies — so native clients (iOS, Android, Unity / Unreal game
// binaries, console SDKs) can speak the same wire contract as the
// SPA. The browser SPA uses Authorization: Bearer on every request
// the same way it would with any third-party API; the token store
// keeps the access token in memory and the refresh token alongside
// it (see frontend/src/services/api.ts). Native clients persist to
// platform-secure storage (Keychain / Keystore / Windows Credential
// Manager / platform SDK save data).
//
// TokenType is always "Bearer" — included for forward-compatibility
// with clients that switch transports later. ExpiresAt is duplicated
// from the JWT's exp claim so clients can drive pre-expiry refresh
// without parsing the token.
type AuthResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type" example:"Bearer"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	User         User      `json:"user"`
}

// RefreshRequest / LogoutRequest carry the refresh token in the
// request body (was previously a __Host-nyx-refresh cookie). The
// required:"true" tag drives huma's automatic 400 response on a
// missing or empty field — the handler never sees a malformed
// payload.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" required:"true"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refresh_token" required:"true"`
}
