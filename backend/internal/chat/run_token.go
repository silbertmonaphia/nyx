package chat

import (
	"context"
	"fmt"
	"strings"

	"nyx/internal/reqctx"
)

type RunTokenMinter interface {
	GenerateToken(userID int, username string) (string, error)
}

func (s *Service) AgentRunToken(ctx context.Context) (string, error) {
	if s.runTokenMinter == nil {
		return "", ErrAgentRunToken
	}
	userID := reqctx.UserIDFromContext(ctx)
	if userID == 0 {
		return "", ErrAgentRunToken
	}
	token, err := s.runTokenMinter.GenerateToken(userID, reqctx.UsernameFromContext(ctx))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAgentRunToken, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", ErrAgentRunToken
	}
	return token, nil
}
