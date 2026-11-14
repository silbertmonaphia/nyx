package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/platform/config"
	"nyx/internal/reqctx"
)

type failingRunTokenMinter struct{}

func (failingRunTokenMinter) GenerateToken(int, string) (string, error) {
	return "", errors.New("mint failed")
}

type stubRunTokenMinter struct {
	token string
	err   error
}

func (s stubRunTokenMinter) GenerateToken(int, string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func newRunTokenTestService(minter RunTokenMinter) *Service {
	cfg := &config.Config{LLMMaxHistoryMessages: 5, LLMMaxMessageChars: 50, LLMMaxTokens: 100}
	svc := NewService(&llmProviderUnused{}, cfg, nil, noop.NewTracerProvider().Tracer("test"))
	svc.runTokenMinter = minter
	return svc
}

func TestService_AgentRunToken_MintsClaimsFromContext(t *testing.T) {
	svc := newRunTokenTestService(newRunTokenService(t))
	ctx := reqctx.WithUserID(context.Background(), 42)
	ctx = reqctx.WithUsername(ctx, "alice")

	token, err := svc.AgentRunToken(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, err := newRunTokenService(t).ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, 42, claims.UserID)
	assert.Equal(t, "alice", claims.Username)
}

func TestService_AgentRunToken_NilMinterReturnsErrAgentRunToken(t *testing.T) {
	svc := newRunTokenTestService(nil)
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := svc.AgentRunToken(ctx)
	require.ErrorIs(t, err, ErrAgentRunToken)
}

func TestService_AgentRunToken_RejectsZeroUserID(t *testing.T) {
	svc := newRunTokenTestService(newRunTokenService(t))

	_, err := svc.AgentRunToken(context.Background())
	require.ErrorIs(t, err, ErrAgentRunToken)
}

func TestService_AgentRunToken_WrapsMinterError(t *testing.T) {
	svc := newRunTokenTestService(failingRunTokenMinter{})
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := svc.AgentRunToken(ctx)
	require.ErrorIs(t, err, ErrAgentRunToken)
}

func TestService_AgentRunToken_RejectsEmptyMintedToken(t *testing.T) {
	svc := newRunTokenTestService(stubRunTokenMinter{token: "  "})
	ctx := reqctx.WithUserID(context.Background(), 42)

	_, err := svc.AgentRunToken(ctx)
	require.ErrorIs(t, err, ErrAgentRunToken)
}
