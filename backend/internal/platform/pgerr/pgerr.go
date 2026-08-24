// Package pgerr translates pgconn.PgError values into domain
// sentinels. The repository layer calls Map on every non-pgx.ErrNoRows
// error from a query and forwards the result; the handler layer
// never sees raw SQLSTATE codes.
//
// Coverage: SQLSTATE 23505 (unique_violation), 23503 (foreign_key_violation),
// 23502 (not_null_violation), and 23514 (check_violation). The handler
// layer maps unique_violation by ConstraintName to specific sentinels;
// the other three are surfaced to handlers as-is via Map returning the
// original error.
//
// Matching strategy: switches on ConstraintName (the canonical index
// or constraint identifier the migration declares). Schema renames
// require updating the constants below; that is the explicit trade for
// fine-grained mapping.
package pgerr

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes we recognize. Other PgError codes pass through Map
// unchanged so the caller can decide whether to translate them at a
// higher layer.
const (
	CodeUniqueViolation    = "23505"
	CodeForeignKeyViolation = "23503"
	CodeNotNullViolation   = "23502"
	CodeCheckViolation     = "23514"
)

// Canonical constraint names from the migrations. The pgerr package
// does not depend on the user/movie packages — these constants are the
// shared vocabulary. Renaming a constraint in a future migration
// requires updating the matching constant here.
//
//nolint:gosec // G101 false positive — these are SQL constraint names, not credentials.
const (
	ConstraintUsersUsername          = "users_username_key"
	ConstraintUsersEmail             = "users_email_key"
	ConstraintRefreshTokensTokenHash = "idx_refresh_tokens_token_hash"
)

// Translation is the decoded SQLSTATE + ConstraintName from a
// pgconn.PgError. Ok is false when err is not a *pgconn.PgError (e.g.
// connection refused, context deadline).
type Translation struct {
	Code       string
	Constraint string
	Ok         bool
}

// Translate extracts SQLSTATE + ConstraintName from a pgconn.PgError.
// Returns Translation{Ok: false} for any non-PgError so callers can
// rely on Ok instead of errors.As at every site.
func Translate(err error) Translation {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return Translation{Ok: false}
	}
	return Translation{
		Code:       pgErr.Code,
		Constraint: pgErr.ConstraintName,
		Ok:         true,
	}
}

// Map returns the matching domain sentinel for a recognized
// pgconn.PgError, or err unchanged when err is nil, non-PgError, or a
// PgError whose Code + ConstraintName do not match a known row.
//
// Recognized today:
//   - 23505 + users_username_key          → user.ErrUsernameTaken
//   - 23505 + users_email_key             → user.ErrEmailTaken
//   - 23505 + idx_refresh_tokens_token_hash → user.ErrRefreshTokenCollision
//
// Domain sentinels are imported lazily through a registered matcher
// (Register) so this package has no upstream dependency on the user
// domain. Callers wire the matcher once at process startup.
//
// Returns nil for a nil input so callers can use Map in expression
// chains without a separate nil check.
func Map(err error) error {
	if err == nil {
		return nil
	}
	tr := Translate(err)
	if !tr.Ok {
		return err
	}
	if tr.Code != CodeUniqueViolation {
		return err
	}
	if m, ok := matchers[tr.Constraint]; ok {
		return m()
	}
	return err
}

// matcher is the zero-arg constructor for a domain sentinel. Returning
// a fresh value per call lets callers wrap with %w in the future
// without aliasing the same sentinel across goroutines.
type matcher func() error

// matchers is the constraint → sentinel registry. Populated by
// Register. Lookup is O(1) at the call site.
var matchers = map[string]matcher{}

// Register binds a SQLSTATE ConstraintName to a domain sentinel
// constructor. Last writer wins on duplicate registrations so tests
// can swap sentinels for fakes without leaking global state between
// runs. Call once at process startup from main.go.
func Register(constraint string, sentinel func() error) {
	matchers[constraint] = sentinel
}