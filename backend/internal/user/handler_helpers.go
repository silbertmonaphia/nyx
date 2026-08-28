package user

// toResponse flattens an AuthResult into the JSON envelope clients
// receive on login / register / refresh. Tokens now ride in the
// body (RFC 6750 Bearer transport); there is no other wire channel
// for them. TokenType is hard-coded to "Bearer" — the only auth
// scheme this API issues.
func (r *AuthResult) toResponse() AuthResponse {
	return AuthResponse{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    r.ExpiresAt,
		User:         r.User,
	}
}
