package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nyx/internal/reqctx"
)

// registerTestSentinel installs a sentinel→mapping pair for the
// duration of a test. Returns the sentinel so the test can assert
// against it. t.Cleanup removes the entry so tests stay
// order-independent.
func registerTestSentinel(t *testing.T, status int, message string) error {
	t.Helper()
	sentinel := errors.New("test-sentinel-" + message)
	RegisterSentinel(sentinel, status, message)
	t.Cleanup(func() { delete(mappings, sentinel) })
	return sentinel
}

func TestMapError_NilError(t *testing.T) {
	if got := MapError(context.Background(), nil, "should not be used"); got != nil {
		t.Errorf("MapError(nil) = %+v, want nil", got)
	}
}

func TestMapError_UserSentinels(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
	}{
		{"UserNotFound", http.StatusNotFound, "User not found"},
		{"UsernameTaken", http.StatusConflict, "Username already taken"},
		{"EmailTaken", http.StatusConflict, "Email already taken"},
		{"InvalidCredentials", http.StatusUnauthorized, "Invalid credentials"},
		{"InvalidRefreshToken", http.StatusUnauthorized, "Invalid refresh token"},
		{"RefreshTokenReuse", http.StatusUnauthorized, "Refresh token revoked"},
		{"RefreshTokenExpired", http.StatusUnauthorized, "Refresh token expired"},
		{"RefreshTokenCollision", http.StatusInternalServerError, "Internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := registerTestSentinel(t, tc.status, tc.message)

			got := MapError(context.Background(), sentinel, "safe detail")
			if got == nil {
				t.Fatalf("MapError returned nil for %v", sentinel)
			}
			if got.Code != tc.status {
				t.Errorf("Code = %d, want %d", got.Code, tc.status)
			}
			if got.Message != tc.message {
				t.Errorf("Message = %q, want %q", got.Message, tc.message)
			}
			if got.Details != nil {
				t.Errorf("Details = %v, want nil for known sentinel", got.Details)
			}
		})
	}
}

func TestMapError_MovieNotFound(t *testing.T) {
	notFound := registerTestSentinel(t, http.StatusNotFound, "Movie not found")

	got := MapError(context.Background(), notFound, "safe")
	if got.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want %d", got.Code, http.StatusNotFound)
	}
	if got.Message != "Movie not found" {
		t.Errorf("Message = %q, want %q", got.Message, "Movie not found")
	}
}

func TestMapError_AuthSentinels(t *testing.T) {
	invalid := registerTestSentinel(t, http.StatusUnauthorized, "Invalid token")
	expired := registerTestSentinel(t, http.StatusUnauthorized, "Token expired")

	gotInvalid := MapError(context.Background(), invalid, "safe")
	if gotInvalid.Code != http.StatusUnauthorized || gotInvalid.Message != "Invalid token" {
		t.Errorf("invalid token: got %+v", gotInvalid)
	}

	gotExpired := MapError(context.Background(), expired, "safe")
	if gotExpired.Code != http.StatusUnauthorized || gotExpired.Message != "Token expired" {
		t.Errorf("expired token: got %+v", gotExpired)
	}
}

func TestMapError_UnknownErrorReturnsSafeDetail(t *testing.T) {
	rawErr := errors.New("pgx: SQLSTATE 42P01 relation does not exist")

	got := MapError(context.Background(), rawErr, "Failed to do the thing")

	if got.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", got.Code)
	}
	if got.Message != "Internal server error" {
		t.Errorf("Message = %q, want %q (no raw err text)", got.Message, "Internal server error")
	}
	if got.Details != "Failed to do the thing" {
		t.Errorf("Details = %v, want the supplied safeDetail", got.Details)
	}
	// Defense-in-depth: raw err text must never appear anywhere in the
	// wire-shape fields that a client could see.
	for _, leak := range []string{rawErr.Error(), "SQLSTATE", "42P01"} {
		if strings.Contains(got.Message, leak) || strings.Contains(fmt.Sprint(got.Details), leak) {
			t.Errorf("MapError leaked %q into wire fields: msg=%q details=%v", leak, got.Message, got.Details)
		}
	}
}

func TestMapError_WrappedSentinel(t *testing.T) {
	// err.Is walks the chain; a fmt.Errorf("...: %w", sentinel) wrap
	// still lands on the right mapping.
	sentinel := registerTestSentinel(t, http.StatusNotFound, "Movie not found")
	wrapped := fmt.Errorf("begin tx: %w", sentinel)

	got := MapError(context.Background(), wrapped, "safe")
	if got == nil || got.Code != http.StatusNotFound {
		t.Fatalf("wrapped MapError = %+v, want 404", got)
	}
}

func TestMapError_PreservesRequestIDInLog(t *testing.T) {
	// The unknown-error branch logs through ClassifyAndLog. The log
	// capture is the same one response_test.go uses (zerolog singleton).
	buf := captureLogger(t)

	rawErr := errors.New("internal boom")
	ctx := reqctx.WithRequestID(context.Background(), "req-map-err-xyz")

	got := MapError(ctx, rawErr, "op failed")
	if got == nil || got.Code != http.StatusInternalServerError {
		t.Fatalf("MapError = %+v, want 500 envelope", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "req-map-err-xyz") {
		t.Errorf("expected request_id req-map-err-xyz in log, got: %s", logged)
	}
	if !strings.Contains(logged, "internal boom") {
		t.Errorf("expected raw err string in log, got: %s", logged)
	}
	// Raw err text must still not be in the wire response.
	if strings.Contains(got.Message, "boom") {
		t.Errorf("wire message leaked raw err text: %q", got.Message)
	}
}

func TestMapError_StampsRequestID(t *testing.T) {
	sentinel := registerTestSentinel(t, http.StatusNotFound, "Movie not found")
	ctx := reqctx.WithRequestID(context.Background(), "req-stamp-1")

	got := MapError(ctx, sentinel, "ignored")
	if got == nil {
		t.Fatal("MapError returned nil")
	}
	if got.RequestID != "req-stamp-1" {
		t.Errorf("RequestID = %q, want %q", got.RequestID, "req-stamp-1")
	}
}

func TestRegisterSentinel_LastWriterWins(t *testing.T) {
	// Same sentinel, two registrations. Most-recent wins — a test
	// that swaps sentinels in should be able to do so without
	// leaking to siblings.
	a := errors.New("dup")
	b := a // same sentinel: second registration must overwrite the first

	RegisterSentinel(a, http.StatusBadRequest, "first")
	RegisterSentinel(b, http.StatusOK, "second")
	got := mappings[a]
	if got.status != http.StatusOK || got.message != "second" {
		t.Errorf("last-writer-wins: got %+v, want {200, \"second\"}", got)
	}
	delete(mappings, a)
}
