package crypto

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:gosec // G101: test fixture, not a real credential.
const testSecret = "nyx-user-llm-encryption-test-secret-0123456789abcdef"

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(testSecret)
	require.NoError(t, err)
	return c
}

func TestCipher_Roundtrip(t *testing.T) {
	c := newTestCipher(t)

	plaintext := "sk-live-abc123-pony"
	token, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, token, "ciphertext must differ from plaintext")

	got, err := c.Decrypt(token)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestCipher_WrongKeyFailsClosed(t *testing.T) {
	c1, err := NewCipher(testSecret)
	require.NoError(t, err)
	c2, err := NewCipher(testSecret + "rotated")
	require.NoError(t, err)

	token, err := c1.Encrypt("sk-live-abc123-pony")
	require.NoError(t, err)

	_, err = c2.Decrypt(token)
	require.Error(t, err, "a different key must fail GCM auth")
	assert.NotErrorIs(t, err, ErrUnknownVersion)
}

func TestCipher_DecryptUnknownVersionFailsClosed(t *testing.T) {
	c := newTestCipher(t)

	for _, token := range []string{
		"",
		"sk-live-abc123-pony",
		"v2:" + base64.StdEncoding.EncodeToString([]byte("whatever")),
		"v1",
	} {
		_, err := c.Decrypt(token)
		require.Error(t, err, "token %q must not decrypt", token)
		assert.ErrorIs(t, err, ErrUnknownVersion, "token %q must surface ErrUnknownVersion", token)
	}

	_, err := c.Decrypt("v1:")
	require.Error(t, err, "a bare v1: prefix with no body must not decrypt")
	assert.NotErrorIs(t, err, ErrUnknownVersion, "v1: carries the known prefix, so the failure is the empty body")
}

func TestCipher_DecryptTamperedCiphertextFailsClosed(t *testing.T) {
	c := newTestCipher(t)

	token, err := c.Encrypt("sk-live-abc123-pony")
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "v1:"))
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xFF
	tampered := "v1:" + base64.StdEncoding.EncodeToString(raw)

	_, err = c.Decrypt(tampered)
	require.Error(t, err, "bit-flipped ciphertext must fail GCM auth")
	assert.NotErrorIs(t, err, ErrUnknownVersion)
}

func TestCipher_EncryptFormat(t *testing.T) {
	c := newTestCipher(t)

	token, err := c.Encrypt("sk-live-abc123-pony")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(token, "v1:"), "token must carry the v1: version prefix")

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "v1:"))
	require.NoError(t, err, "body after the prefix must be valid standard base64")
	assert.Equal(t, 12, len(raw)-len("sk-live-abc123-pony")-16,
		"body layout must be nonce(12) || ciphertext || GCM tag(16)")
}

func TestCipher_EncryptFreshNoncePerCall(t *testing.T) {
	c := newTestCipher(t)

	t1, err := c.Encrypt("same-plaintext")
	require.NoError(t, err)
	t2, err := c.Encrypt("same-plaintext")
	require.NoError(t, err)

	assert.NotEqual(t, t1, t2, "each encryption must use a fresh random nonce")
}

func TestNewCipher_RejectsShortKey(t *testing.T) {
	_, err := NewCipher(strings.Repeat("x", MinKeyBytes-1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "USER_LLM_ENCRYPTION_KEY")
}

func TestNewCipher_RejectsEmptyKey(t *testing.T) {
	_, err := NewCipher("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "USER_LLM_ENCRYPTION_KEY")
}

func TestNewCipher_AcceptsKeyAtMinLength(t *testing.T) {
	_, err := NewCipher(strings.Repeat("x", MinKeyBytes))
	require.NoError(t, err)
}
