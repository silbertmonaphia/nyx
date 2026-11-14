package kb

import "nyx/internal/platform/config"

func ConfigFrom(c *config.Config) Config {
	return Config{
		Enabled:          c.KBEnabled,
		Root:             c.KBRoot,
		SharedSourceName: c.KBSharedSourceName,
		Chunk:            ChunkOptionsFrom(c),
		TopK:             c.KBTopK,
		MaxContextChars:  c.KBMaxContextChars,
		IndexConcurrency: c.KBIndexConcurrency,
	}
}

func ChunkOptionsFrom(c *config.Config) ChunkOptions {
	return ChunkOptions{
		TargetTokens:  c.KBChunkTargetTokens,
		MaxTokens:     c.KBChunkMaxTokens,
		MinTokens:     c.KBChunkMinTokens,
		OverlapTokens: c.KBChunkOverlapTokens,
	}
}

func ConnectorOptionsFrom(c *config.Config) ConnectorOptions {
	return ConnectorOptions{
		MaxFileBytes: c.KBMaxFileBytes,
		MaxFiles:     c.KBMaxFiles,
		MaxDepth:     c.KBMaxDepth,
	}
}
