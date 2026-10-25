package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	MinKeyBytes = 32

	versionPrefix = "v1:"
)

var ErrUnknownVersion = errors.New("crypto: unknown ciphertext version prefix")

type Cipher struct {
	aead cipher.AEAD
}

func NewCipher(secret string) (*Cipher, error) {
	if secret == "" {
		return nil, fmt.Errorf("crypto: USER_LLM_ENCRYPTION_KEY is required")
	}
	if len(secret) < MinKeyBytes {
		return nil, fmt.Errorf("crypto: USER_LLM_ENCRYPTION_KEY must be at least %d bytes, got %d", MinKeyBytes, len(secret))
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("crypto: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher.NewGCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce generation failed: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return versionPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) Decrypt(token string) (string, error) {
	if !strings.HasPrefix(token, versionPrefix) {
		return "", ErrUnknownVersion
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, versionPrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: ciphertext is not valid base64: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", fmt.Errorf("crypto: ciphertext is shorter than the nonce size")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decryption failed: %w", err)
	}
	return string(plaintext), nil
}
