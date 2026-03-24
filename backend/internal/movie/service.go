package movie

import (
	"context"
)

type Service interface {
	GetMovies(ctx context.Context, query string) ([]Movie, error)
	CreateMovie(ctx context.Context, m *Movie) error
	UpdateMovie(ctx context.Context, id int, m *Movie) error
	DeleteMovie(ctx context.Context, id int) error
	CheckHealth(ctx context.Context) error
}

type movieService struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &movieService{repo: repo}
}

func (s *movieService) GetMovies(ctx context.Context, query string) ([]Movie, error) {
	return s.repo.GetAll(ctx, query)
}

func (s *movieService) CreateMovie(ctx context.Context, m *Movie) error {
	return s.repo.Create(ctx, m)
}

func (s *movieService) UpdateMovie(ctx context.Context, id int, m *Movie) error {
	return s.repo.Update(ctx, id, m)
}

func (s *movieService) DeleteMovie(ctx context.Context, id int) error {
	return s.repo.Delete(ctx, id)
}

func (s *movieService) CheckHealth(ctx context.Context) error {
	return s.repo.Ping(ctx)
}
