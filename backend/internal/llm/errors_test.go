package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestUpstreamStatusErrorWrapsSentinel(t *testing.T) {
	carrier := NewUpstreamStatus(401, fmt.Sprintf("upstream %d: %v", 401, ErrProviderUnavailable), ErrProviderUnavailable)
	if !errors.Is(carrier, ErrProviderUnavailable) {
		t.Fatalf("errors.Is must reach the wrapped sentinel, got %v", carrier)
	}
	if got, want := carrier.Error(), "upstream 401: llm provider unavailable"; got != want {
		t.Fatalf("message must stay byte-identical to the fmt.Errorf form: got %q want %q", got, want)
	}
	if status, ok := UpstreamStatusOf(fmt.Errorf("outer: %w", carrier)); !ok || status != 401 {
		t.Fatalf("UpstreamStatusOf must walk the chain: got status %d ok=%v", status, ok)
	}
}

func TestUpstreamStatusOfZeroStatusIsAbsent(t *testing.T) {
	carrier := NewUpstreamStatus(0, fmt.Sprintf("upstream unreachable: %v", ErrProviderUnavailable), ErrProviderUnavailable)
	if _, ok := UpstreamStatusOf(carrier); ok {
		t.Fatal("a zero status must not be reported as present")
	}
	if !errors.Is(carrier, ErrProviderUnavailable) {
		t.Fatalf("zero-status carrier must still unwrap to its sentinel, got %v", carrier)
	}
}

func TestUpstreamStatusOfPlainError(t *testing.T) {
	if _, ok := UpstreamStatusOf(ErrRateLimited); ok {
		t.Fatal("a bare sentinel carries no upstream status")
	}
}
