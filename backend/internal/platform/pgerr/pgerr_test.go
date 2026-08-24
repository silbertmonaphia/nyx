package pgerr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTranslate_UniqueViolationWithConstraintName(t *testing.T) {
	err := &pgconn.PgError{Code: CodeUniqueViolation, ConstraintName: ConstraintUsersUsername}
	tr := Translate(err)
	if !tr.Ok {
		t.Fatalf("Translate: Ok = false, want true")
	}
	if tr.Code != CodeUniqueViolation {
		t.Errorf("Code = %q, want %q", tr.Code, CodeUniqueViolation)
	}
	if tr.Constraint != ConstraintUsersUsername {
		t.Errorf("Constraint = %q, want %q", tr.Constraint, ConstraintUsersUsername)
	}
}

func TestTranslate_UniqueViolationWithoutConstraintName(t *testing.T) {
	// Some unique-violation paths (deferrable constraints, partial
	// indexes) may arrive with an empty ConstraintName. Map still
	// returns ok=true so the caller can decide whether to fall
	// through to a generic 409.
	err := &pgconn.PgError{Code: CodeUniqueViolation, ConstraintName: ""}
	tr := Translate(err)
	if !tr.Ok {
		t.Fatalf("Translate: Ok = false, want true (SQLSTATE was set)")
	}
	if tr.Code != CodeUniqueViolation {
		t.Errorf("Code = %q, want %q", tr.Code, CodeUniqueViolation)
	}
	if tr.Constraint != "" {
		t.Errorf("Constraint = %q, want empty", tr.Constraint)
	}
}

func TestTranslate_FKViolation(t *testing.T) {
	err := &pgconn.PgError{Code: CodeForeignKeyViolation, ConstraintName: "fk_whatever"}
	tr := Translate(err)
	if !tr.Ok {
		t.Fatalf("Translate: Ok = false, want true")
	}
	if tr.Code != CodeForeignKeyViolation {
		t.Errorf("Code = %q, want %q", tr.Code, CodeForeignKeyViolation)
	}
}

func TestTranslate_NotNullViolation(t *testing.T) {
	err := &pgconn.PgError{Code: CodeNotNullViolation, ColumnName: "username"}
	tr := Translate(err)
	if !tr.Ok || tr.Code != CodeNotNullViolation {
		t.Errorf("Translate = %+v, want Ok=true Code=%q", tr, CodeNotNullViolation)
	}
}

func TestTranslate_CheckViolation(t *testing.T) {
	err := &pgconn.PgError{Code: CodeCheckViolation, ConstraintName: "rating_range"}
	tr := Translate(err)
	if !tr.Ok || tr.Code != CodeCheckViolation {
		t.Errorf("Translate = %+v, want Ok=true Code=%q", tr, CodeCheckViolation)
	}
}

func TestTranslate_NonPgError(t *testing.T) {
	err := errors.New("connection refused")
	tr := Translate(err)
	if tr.Ok {
		t.Errorf("Translate: Ok = true on plain error, want false")
	}
	if tr.Code != "" || tr.Constraint != "" {
		t.Errorf("Translate = %+v, want zero-valued fields", tr)
	}
}

func TestTranslate_WrappedPgError(t *testing.T) {
	// Translate must walk the wrap chain via errors.As so callers
	// don't need to peel fmt.Errorf("%w", pgErr) themselves.
	inner := &pgconn.PgError{Code: CodeUniqueViolation, ConstraintName: ConstraintUsersUsername}
	wrapped := fmt.Errorf("begin tx: %w", inner)
	tr := Translate(wrapped)
	if !tr.Ok || tr.Constraint != ConstraintUsersUsername {
		t.Errorf("wrapped Translate = %+v, want Ok=true Constraint=%q", tr, ConstraintUsersUsername)
	}
}

func TestMap_UnknownPgErrorReturnsOriginal(t *testing.T) {
	// A 40001 (serialization_failure) is a recognized PgError SQLSTATE
	// but we don't map it. Map must return the original error so
	// callers keep full context for retry logic.
	original := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
	got := Map(original)
	if !errors.Is(got, original) {
		t.Errorf("Map returned %v, want the original error", got)
	}
}

func TestMap_NonPgErrorReturnsOriginal(t *testing.T) {
	original := errors.New("connection refused")
	got := Map(original)
	if !errors.Is(got, original) {
		t.Errorf("Map returned %v, want the original error", got)
	}
}

func TestMap_NilErrorReturnsNil(t *testing.T) {
	if got := Map(nil); got != nil {
		t.Errorf("Map(nil) = %v, want nil", got)
	}
}

func TestMap_NoMatcherPassesThrough(t *testing.T) {
	// With no Register calls active for this constraint (the global
	// registry may carry stale entries from sibling tests), an
	// unmapped unique violation falls through unchanged.
	err := &pgconn.PgError{Code: CodeUniqueViolation, ConstraintName: "totally_unknown_constraint_xyz"}
	got := Map(err)
	if !errors.Is(got, err) {
		t.Errorf("Map with no matcher returned %v, want original", got)
	}
}

// TestMap_RegisteredMatcherReturnsSentinel exercises the Register→Map
// round-trip. Each subtest uses a unique constraint name and a fresh
// sentinel so they don't share state through the package-level
// matchers map.
func TestMap_RegisteredMatcherReturnsSentinel(t *testing.T) {
	want := errors.New("domain sentinel for the test")
	Register("test_unique_constraint_a", func() error { return want })

	pgErr := &pgconn.PgError{Code: CodeUniqueViolation, ConstraintName: "test_unique_constraint_a"}
	got := Map(pgErr)
	if !errors.Is(got, want) {
		t.Errorf("Map = %v, want the registered sentinel %v", got, want)
	}

	// Clean up so we don't leak the registration to sibling packages'
	// tests when this file runs in -count=N mode.
	delete(matchers, "test_unique_constraint_a")
}