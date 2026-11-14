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

	"go.opentelemetry.io/otel/trace"

	"nyx/internal/kb"
	"nyx/internal/platform/config"
	"nyx/internal/rag"
)

var (
	errRootRequired    = errors.New("--root is required")
	errEmbeddingConfig = errors.New("embedding configuration missing: set LLM_ENABLED=true with LLM_BASE_URL and LLM_MODEL, or LLM_EMBEDDING_BASE_URL and LLM_EMBEDDING_MODEL")
)

type indexOptions struct {
	Root       string
	SourceName string
}

type indexService interface {
	IndexSource(ctx context.Context, src kb.Source) (kb.ScanResult, error)
}

type sharedSourceStore interface {
	UpsertSharedSource(ctx context.Context, name, rootPath string) (kb.Source, error)
}

type statusStore interface {
	ListAllSources(ctx context.Context) ([]kb.Source, error)
	SourceStats(ctx context.Context, id int64) (kb.SourceStats, error)
}

type statusSource struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Enabled   bool   `json:"enabled"`
	Documents int64  `json:"documents"`
	Chunks    int64  `json:"chunks"`
	Pending   int64  `json:"pending"`
}

type statusReport struct {
	EmbeddingModel string         `json:"embedding_model"`
	Sources        []statusSource `json:"sources"`
}

func parseIndexOptions(args []string) (indexOptions, error) {
	flags := flag.NewFlagSet("kb index", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "")
	if err := flags.Parse(args); err != nil {
		return indexOptions{}, err
	}
	if *root == "" {
		return indexOptions{}, errRootRequired
	}
	return indexOptions{Root: *root}, nil
}

func deploymentEmbedder(cfg *config.Config, tracerProvider trace.TracerProvider) (*rag.Embedder, error) {
	embedder, err := rag.NewDeploymentEmbedder(cfg, tracerProvider)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errEmbeddingConfig, err)
	}
	return embedder, nil
}

func runIndexWith(ctx context.Context, opts indexOptions, out io.Writer, svc indexService, shared sharedSourceStore, embedder kb.Embedder) error {
	if embedder == nil {
		return errEmbeddingConfig
	}
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return err
	}
	var src kb.Source
	name := opts.SourceName
	if name == "" {
		name = "shared"
	}
	_, _ = fmt.Fprintf(out, "about to make %s visible to every user as shared source %q\n", root, name)
	src, err = shared.UpsertSharedSource(ctx, name, root)
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

func runStatusWith(ctx context.Context, out io.Writer, store statusStore, model string) error {
	sources, err := store.ListAllSources(ctx)
	if err != nil {
		return err
	}
	report := statusReport{EmbeddingModel: model, Sources: make([]statusSource, 0, len(sources))}
	for _, src := range sources {
		stats, err := store.SourceStats(ctx, src.ID)
		if err != nil {
			return err
		}
		report.Sources = append(report.Sources, statusSource{
			ID:        src.ID,
			Scope:     src.Scope,
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
