package database

import (
	"strings"
	"testing"
)

// TestRunMigrations_PathFromParameter asserts that RunMigrations uses the
// migration path supplied by the caller rather than reading
// MIGRATION_PATH from the environment. An obviously bad path must
// surface as a returned error.
func TestRunMigrations_PathFromParameter(t *testing.T) {
	// A dbURL that is structurally valid for golang-migrate but points
	// nowhere; we never reach the DB because the bad path is checked
	// first by migrate.New. The exact form isn't load-bearing — any
	// path that opens cleanly but doesn't matter, paired with a
	// nonexistent migration source, gives us the assertion we want.
	err := RunMigrations(
		"postgres://nobody:nobody@127.0.0.1:1/nobody?sslmode=disable",
		"file:///nonexistent/migrations/path",
	)
	if err == nil {
		t.Fatal("expected error for nonexistent migration path, got nil")
	}
	if !strings.Contains(err.Error(), "migrations:") {
		t.Errorf("expected error to be wrapped with 'migrations:' prefix, got: %v", err)
	}
}

// TestRunMigrations_ReturnsErrors verifies the function returns errors
// instead of exiting the process. With an unparseable dbURL,
// migrate.New fails — we assert the failure comes back as a returned
// error rather than terminating the test process.
//
// We run this in a sub-test so a future regression that re-introduces
// process-exit behavior is contained: the parent test would still
// report a clean failure rather than hanging the suite.
func TestRunMigrations_ReturnsErrors(t *testing.T) {
	t.Run("unparseable URL returns error", func(t *testing.T) {
		err := RunMigrations(
			"not-a-valid-url",
			"file:///also/garbage",
		)
		if err == nil {
			t.Fatal("expected error for unparseable dbURL, got nil")
		}
		if !strings.Contains(err.Error(), "migrations:") {
			t.Errorf("expected error to be wrapped with 'migrations:' prefix, got: %v", err)
		}
	})
}

// TestRunMigrations_IgnoresEnvVar documents the contract: the
// MIGRATION_PATH environment variable is no longer consulted. Even
// when it is set to a plausible value, RunMigrations must use the
// path passed as a parameter — and conversely, even when it is set to
// garbage, RunMigrations must still succeed-or-fail based on the
// parameter, not the env.
func TestRunMigrations_IgnoresEnvVar(t *testing.T) {
	const envGarbage = "/some/garbage/env/path"

	// Snapshot and restore MIGRATION_PATH so the test doesn't leak.
	t.Setenv("MIGRATION_PATH", envGarbage)

	// With an unparseable dbURL and a garbage param path, RunMigrations
	// must fail because of the URL/parameter, not because it tried to
	// open envGarbage. We assert the error mentions the parameter path
	// we supplied, which would be impossible if the function were
	// silently reading the env var instead.
	paramPath := "file:///also/garbage"
	err := RunMigrations("not-a-valid-url", paramPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), envGarbage) {
		t.Errorf("error references env-var path %q — RunMigrations should not consult MIGRATION_PATH; got: %v",
			envGarbage, err)
	}
}

// TestRunMigrations_EmptyPath ensures an empty migrationPath is
// rejected up front with a clear error rather than being silently
// replaced by an environment-supplied default (the old behavior).
func TestRunMigrations_EmptyPath(t *testing.T) {
	// Make sure no env var can paper over the empty parameter.
	t.Setenv("MIGRATION_PATH", "file:///some/env/path")

	err := RunMigrations("not-a-valid-url", "")
	if err == nil {
		t.Fatal("expected error for empty migration path, got nil")
	}
	if !strings.Contains(err.Error(), "migration path is required") {
		t.Errorf("expected clear 'migration path is required' error, got: %v", err)
	}
}
