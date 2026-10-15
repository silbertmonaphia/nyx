package tool

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFieldError_ErrorAndUnwrap pins both rendering branches: with a
// field name the message is prefixed ("Title: required"); without one
// (the validator's structural errors) the inner message stands alone.
// errors.Is must walk through Unwrap to the inner error.
func TestFieldError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("required")

	withField := &FieldError{Field: "Title", Err: inner}
	assert.Equal(t, "Title: required", withField.Error())
	assert.True(t, errors.Is(withField, inner), "Unwrap must expose the inner error")

	bare := &FieldError{Err: inner}
	assert.Equal(t, "required", bare.Error())
	assert.True(t, errors.Is(bare, inner))
}
