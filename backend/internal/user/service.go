package user

import (
	"context"
	"errors"
	"nyx/internal/platform/auth"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
)

type Service interface {
	Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error)
	Login(ctx context.Context, req LoginRequest) (*AuthResponse, error)
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error) {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	u := &User{
		Username:     req.Username,
		Email:        req.Email,
		PasswordHash: string(hashedPassword),
	}

	if err := s.repo.CreateUser(ctx, u); err != nil {
		return nil, err
	}

	token, err := auth.GenerateToken(u.ID, u.Username)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token: token,
		User:  *u,
	}, nil
}

func (s *service) Login(ctx context.Context, req LoginRequest) (*AuthResponse, error) {
	u, err := s.repo.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	token, err := auth.GenerateToken(u.ID, u.Username)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token: token,
		User:  *u,
	}, nil
}
