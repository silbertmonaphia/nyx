package user

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// newRefreshToken mints an opaque refresh token: 32 bytes from
// crypto/rand, base64url (RawURLEncoding — no padding). The DB stores
// only the SHA-256 hash; the raw bytes never persist. Returning the
// raw value lets the caller hand it back to the client (once) and the
// hash to the repository.
//
// 32 bytes = 256 bits of entropy. base64url expands that to a 43-char
// ASCII string — long enough to make brute-force guessing on a
// leaked hash infeasible and short enough to fit in a header without
// further encoding.
//
// Errors are reserved for crypto/rand failures (e.g. on a broken
// embedded device). Production builds on linux/amd64 — rand.Read is
// documented never to fail on Linux — so this path is unreachable in
// normal operation, but we surface it rather than panic so the
// service layer's error mapping stays consistent.
func newRefreshToken() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("read random bytes: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)

	sum := sha256.Sum256([]byte(raw))
	hash = sum[:]
	return raw, hash, nil
}

// sha256Sum hashes a raw refresh token for storage/lookup. The hash
// is the only thing that ever lands in the DB — never persist the
// raw value. Kept as a package-level helper so the service layer can
// hash on lookup without importing crypto/sha256 directly.
func sha256Sum(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
