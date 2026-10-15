package chat

import (
	"context"
	"testing"
)

func TestDetachContext_SurvivesParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	detached := detachContext(parent)
	if err := detached.Err(); err != nil {
		t.Fatalf("detached context must not inherit the parent's cancellation: %v", err)
	}
	select {
	case <-detached.Done():
		t.Fatal("detached context must stay live for the backfill window")
	default:
	}
}

func TestDetachContext_CarriesDeadline(t *testing.T) {
	detached := detachContext(context.Background())
	if _, ok := detached.Deadline(); !ok {
		t.Fatal("detached context must carry the backfill deadline")
	}
}
