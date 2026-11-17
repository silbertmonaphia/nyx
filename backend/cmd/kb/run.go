package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/llmsettings"
	"nyx/internal/platform/config"
	"nyx/internal/rag"
)

var (
	errUserRequired    = errors.New("--user is required")
	errRootRequired    = errors.New("--root is required")
	errEmbeddingConfig = errors.New("embedding configuration missing for this user: save a BYOK embedding configuration first")
)

const maxSourceNameRunes = 200

type indexOptions struct {
	UserID int64
	Root   string
	Name   string
}

type indexService interface {
	IndexSource(ctx context.Context, src kb.Source) (kb.ScanResult, error)
}

type dirSourceStore interface {
	UpsertDirSource(ctx context.Context, ownerID int64, name, rootPath string) (kb.Source, error)
}

type embeddingResolver interface {
	Resolve(ctx context.Context, userID int) (*llmsettings.UserStack, error)
}

type statusStore interface {
	ListAllSources(ctx context.Context) ([]kb.Source, error)
	SourceStats(ctx context.Context, id int64) (kb.SourceStats, error)
}

type statusSource struct {
	ID        int64  `json:"id"`
	OwnerID   int64  `json:"owner_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Enabled   bool   `json:"enabled"`
	Documents int64  `json:"documents"`
	Chunks    int64  `json:"chunks"`
	Pending   int64  `json:"pending"`
}

type statusReport struct {
	Sources []statusSource `json:"sources"`
}

func parseIndexOptions(args []string) (indexOptions, error) {
	flags := flag.NewFlagSet("kb index", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	user := flags.String("user", "", "")
	root := flags.String("root", "", "")
	name := flags.String("name", "", "")
	if err := flags.Parse(args); err != nil {
		return indexOptions{}, err
	}
	if strings.TrimSpace(*user) == "" {
		return indexOptions{}, errUserRequired
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(*user), 10, 64)
	if err != nil || userID <= 0 {
		return indexOptions{}, fmt.Errorf("--user must be a positive integer user id")
	}
	if *root == "" {
		return indexOptions{}, errRootRequired
	}
	sourceName, err := validateSourceName(*name)
	if err != nil {
		return indexOptions{}, err
	}
	return indexOptions{UserID: userID, Root: *root, Name: sourceName}, nil
}

func validateSourceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if len([]rune(name)) > maxSourceNameRunes {
		return "", fmt.Errorf("--name must be at most %d characters", maxSourceNameRunes)
	}
	if strings.EqualFold(name, "inline") {
		return "", fmt.Errorf("--name %q is reserved for the inline source", name)
	}
	return name, nil
}

func resolveUserEmbedder(ctx context.Context, cfg *config.Config, resolver embeddingResolver, userID int64) (kb.Embedder, error) {
	stack, err := resolver.Resolve(ctx, int(userID))
	if errors.Is(err, llmsettings.ErrNotFound) {
		return nil, fmt.Errorf("user %d: %w", userID, errEmbeddingConfig)
	}
	if err != nil {
		return nil, err
	}
	if stack == nil || stack.Embed == nil || stack.EmbedModel == "" {
		return nil, fmt.Errorf("user %d: %w", userID, errEmbeddingConfig)
	}
	embedder := rag.NewEmbedder(stack.Embed, stack.EmbedModel, noop.NewTracerProvider().Tracer("nyx.kb.cli"))
	embedder.SetDimensions(cfg.EmbeddingDimensions)
	return embedder, nil
}

func runIndexCommand(ctx context.Context, cfg *config.Config, store kb.Store, resolver embeddingResolver, args []string) error {
	opts, err := parseIndexOptions(args)
	if err != nil {
		return err
	}
	embedder, err := resolveUserEmbedder(ctx, cfg, resolver, opts.UserID)
	if err != nil {
		return err
	}
	indexer := kb.NewIndexer(store, kb.ChunkOptionsFrom(cfg), kb.ConnectorOptionsFrom(cfg))
	indexer.SetEmbedderResolver(func(context.Context, int64) (kb.Embedder, error) {
		return embedder, nil
	})
	service := kb.NewService(store, indexer, kb.ConfigFrom(cfg), noop.NewTracerProvider().Tracer("nyx.kb.cmd"))
	return runIndexWith(ctx, opts, os.Stdout, service, store, embedder)
}

func runIndexWith(ctx context.Context, opts indexOptions, out io.Writer, svc indexService, store dirSourceStore, embedder kb.Embedder) error {
	if embedder == nil {
		return errEmbeddingConfig
	}
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return err
	}
	name := opts.Name
	if name == "" {
		name = filepath.Base(root)
	}
	name, err = validateSourceName(name)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "indexing user=%d source=%q root=%s\n", opts.UserID, name, root)
	src, err := store.UpsertDirSource(ctx, opts.UserID, name, root)
	if err != nil {
		return err
	}
	result, err := svc.IndexSource(ctx, src)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "source=%s documents=%d chunks=%d skipped=%d\n",
		src.Name, result.Documents, result.Chunks, len(result.Skipped))
	return nil
}

func runStatusWith(ctx context.Context, out io.Writer, store statusStore) error {
	sources, err := store.ListAllSources(ctx)
	if err != nil {
		return err
	}
	report := statusReport{Sources: make([]statusSource, 0, len(sources))}
	for _, src := range sources {
		stats, err := store.SourceStats(ctx, src.ID)
		if err != nil {
			return err
		}
		report.Sources = append(report.Sources, statusSource{
			ID:        src.ID,
			OwnerID:   src.OwnerID,
			Name:      src.Name,
			Kind:      src.Kind,
			Enabled:   src.Enabled,
			Documents: stats.Documents,
			Chunks:    stats.Chunks,
			Pending:   stats.Pending,
		})
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func resolveRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	return resolved, nil
}
