package main

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/platform/config"
	"nyx/internal/platform/database"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kb:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kb <index|status> [flags]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if migErr := database.RunMigrations(cfg.DBURL, cfg.MigrationPath); migErr != nil {
		return fmt.Errorf("migrations failed: %w", migErr)
	}
	ctx := context.Background()
	pool, err := database.New(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer pool.Close()

	tracerProvider := noop.NewTracerProvider()
	store := kb.NewSQLStore(pool)
	embedder, embedErr := deploymentEmbedder(cfg, tracerProvider)

	switch args[0] {
	case "index":
		return runIndexCommand(ctx, cfg, store, embedder, embedErr, args[1:])
	case "status":
		model := ""
		if embedder != nil {
			model = embedder.Model()
		}
		return runStatusWith(ctx, os.Stdout, store, model)
	default:
		return fmt.Errorf("unknown subcommand %q (usage: kb <index|status>)", args[0])
	}
}

func runIndexCommand(ctx context.Context, cfg *config.Config, store kb.Store, embedder kb.Embedder, embedErr error, args []string) error {
	opts, err := parseIndexOptions(args)
	if err != nil {
		return err
	}
	opts.SourceName = cfg.KBSharedSourceName
	if embedErr != nil {
		return embedErr
	}
	indexer := kb.NewIndexer(store, embedder, kb.ChunkOptionsFrom(cfg), kb.ConnectorOptionsFrom(cfg))
	service := kb.NewService(store, indexer, kb.ConfigFrom(cfg), embedder, noop.NewTracerProvider().Tracer("nyx.kb.cmd"))
	return runIndexWith(ctx, opts, os.Stdout, service, store, embedder)
}
