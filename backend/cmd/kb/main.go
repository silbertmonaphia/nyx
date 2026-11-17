package main

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/llmsettings"
	"nyx/internal/platform/config"
	"nyx/internal/platform/crypto"
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

	switch args[0] {
	case "index":
		settingsCipher, cipherErr := crypto.NewCipher(cfg.UserLLMEncryptionKey)
		if cipherErr != nil {
			return fmt.Errorf("invalid USER_LLM_ENCRYPTION_KEY: %w", cipherErr)
		}
		settingsService := llmsettings.NewService(llmsettings.NewRepository(pool), settingsCipher)
		resolver := llmsettings.NewResolver(settingsService, cfg, tracerProvider)
		return runIndexCommand(ctx, cfg, store, resolver, args[1:])
	case "status":
		return runStatusWith(ctx, os.Stdout, store)
	default:
		return fmt.Errorf("unknown subcommand %q (usage: kb <index|status>)", args[0])
	}
}
